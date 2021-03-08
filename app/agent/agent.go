package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	b64 "encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/opsmx/oes-birger/pkg/kubeconfig"
	"github.com/opsmx/oes-birger/pkg/tunnel"
)

var (
	tickTime           = flag.Int("tickTime", 30, "Time between sending Ping messages")
	agentCertFile      = flag.String("certFile", "/app/secrets/agent/tls.crt", "The file containing the certificate used to connect to the controller")
	agentKeyFile       = flag.String("keyFile", "/app/secrets/agent/tls.key", "The file containing the certificate used to connect to the controller")
	caCertFile         = flag.String("caCertFile", "/app/config/ca.pem", "The file containing the CA certificate we will use to verify the controller's cert")
	kubeConfigFilename = flag.String("kubeconfig", "/app/config/kubeconfig.yaml", "The location of a kubeconfig file to define endpoints and kube API auth")
	configFile         = flag.String("configFile", "/app/config/config.yaml", "The file with the controller config")

	emptyBytes = []byte("")

	config *AgentConfig
)

func runKubernetesTunnel(wg *sync.WaitGroup, sa *serverContext, conn *grpc.ClientConn) {
	defer wg.Done()

	ticker := time.NewTicker(time.Duration(*tickTime) * time.Second)

	client := tunnel.NewAgentTunnelServiceClient(conn)
	ctx := context.Background()

	stream, err := client.EventTunnel(ctx)
	if err != nil {
		log.Fatalf("%v.EventTunnel(_) = _, %v", client, err)
	}
	hello := &tunnel.AgentToControllerWrapper{
		Event: &tunnel.AgentToControllerWrapper_AgentHello{
			AgentHello: &tunnel.AgentHello{
				Protocols:            []string{"kubernetes", "remote-command"},
				CommandNames:         []string{"bash"},
				KubernetesNamespaces: config.Namespaces,
				ProtocolVersion:      tunnel.CurrentProtocolVersion,
			},
		},
	}
	if err = stream.Send(hello); err != nil {
		log.Fatalf("Unable to send hello packet: %v", err)
	}

	dataflow := make(chan *tunnel.AgentToControllerWrapper, 20)

	// Handle periodic pings from the ticker.
	go func() {
		for ts := range ticker.C {
			req := &tunnel.AgentToControllerWrapper{
				Event: &tunnel.AgentToControllerWrapper_PingRequest{
					PingRequest: &tunnel.PingRequest{Ts: uint64(ts.UnixNano())},
				},
			}
			if err = stream.Send(req); err != nil {
				log.Fatalf("Unable to send a PingRequest: %v", err)
			}
		}
	}()

	// Handle data flowing back to the controller
	go func() {
		for ew := range dataflow {
			if err = stream.Send(ew); err != nil {
				log.Printf("Unable to respond over GRPC: %v", err)
			}
		}
	}()

	waitc := make(chan struct{})
	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				// Server has closed the connection.
				close(waitc)
				return
			}
			if err != nil {
				log.Fatalf("Failed to receive a message: %T: %v", err, err)
			}
			switch x := in.Event.(type) {
			case *tunnel.ControllerToAgentWrapper_PingResponse:
				continue
			case *tunnel.ControllerToAgentWrapper_CancelRequest:
				req := in.GetCancelRequest()
				callCancelFunction(req.Id)
			case *tunnel.ControllerToAgentWrapper_HttpRequest:
				req := in.GetHttpRequest()
				if req.Protocol == "kubernetes" {
					go executeKubernetesRequest(dataflow, makeServerContextFields(sa), req)
				} else {
					log.Printf("Request for unsupported HTTP tunnel: %s", req.Protocol)
					dataflow <- makeBadGatewayResponse(req.Id, req.Target)
				}
			case *tunnel.ControllerToAgentWrapper_CommandRequest:
				req := in.GetCommandRequest()
				switch req.Name {
				case "bash":
					go runCommand(dataflow, req)
				default:
					dataflow <- makeCommandFailed(req, nil, "Agent: Unknown command")
				}
			case nil:
				continue
			default:
				log.Printf("Received unknown message: %T", x)
			}
		}
	}()
	<-waitc
	close(dataflow)
	stream.CloseSend()
}

type serverContextFields struct {
	username   string
	serverURL  string
	serverCA   *x509.Certificate
	clientCert *tls.Certificate
	token      string
	insecure   bool
}

func (scf *serverContextFields) isSameAs(scf2 *serverContextFields) bool {
	if scf.username != scf2.username || scf.serverURL != scf2.serverURL || scf.token != scf2.token || scf.insecure != scf2.insecure {
		return false
	}

	if (scf.serverCA == nil && scf2.serverCA != nil) || (scf.serverCA != nil && scf2.serverCA == nil) {
		return false
	}
	if scf.serverCA != nil && scf2.serverCA != nil {
		if !scf.serverCA.Equal(scf2.serverCA) {
			return false
		}
	}

	if (scf.clientCert == nil && scf2.clientCert != nil) || (scf.clientCert != nil && scf2.clientCert == nil) {
		return false
	}
	if scf.clientCert != nil && scf2.clientCert != nil {
		if bytes.Compare(scf.clientCert.Certificate[0], scf2.clientCert.Certificate[0]) != 0 {
			return false
		}
	}

	return true
}

type serverContext struct {
	sync.RWMutex
	f serverContextFields
}

func makeServerContextFields(src *serverContext) *serverContextFields {
	src.RLock()
	defer src.RUnlock()
	return &serverContextFields{
		username:   src.f.username,
		serverURL:  src.f.serverURL,
		serverCA:   src.f.serverCA,
		clientCert: src.f.clientCert,
		token:      src.f.token,
		insecure:   src.f.insecure,
	}
}

func serverContextFromKubeconfig(kconfig *kubeconfig.KubeConfig) *serverContextFields {
	names := kconfig.GetContextNames()
	for _, name := range names {
		if name != kconfig.CurrentContext {
			continue
		}
		user, cluster, err := kconfig.FindContext(name)
		if err != nil {
			log.Fatalf("Unable to retrieve cluster and user info for context %s: %v", name, err)
		}

		certData, err := b64.StdEncoding.DecodeString(user.User.ClientCertificateData)
		if err != nil {
			log.Fatalf("Error decoding user cert from base64 (%s): %v", user.Name, err)
		}
		keyData, err := b64.StdEncoding.DecodeString(user.User.ClientKeyData)
		if err != nil {
			log.Fatalf("Error decoding user key from base64 (%s): %v", user.Name, err)
		}

		clientKeypair, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			log.Fatalf("Error loading client cert/key: %v", err)
		}

		saf := &serverContextFields{
			username:   user.Name,
			clientCert: &clientKeypair,
			serverURL:  cluster.Cluster.Server,
			insecure:   cluster.Cluster.InsecureSkipTLSVerify,
		}

		if len(cluster.Cluster.CertificateAuthorityData) > 0 {
			serverCA, err := b64.StdEncoding.DecodeString(cluster.Cluster.CertificateAuthorityData)
			if err != nil {
				log.Fatalf("Error decoding server CA cert from base64 (%s): %v", cluster.Name, err)
			}
			pemBlock, _ := pem.Decode(serverCA)
			serverCert, err := x509.ParseCertificate(pemBlock.Bytes)
			if err != nil {
				log.Fatalf("Error parsing server certificate: %v", err)
			}
			saf.serverCA = serverCert
		}

		return saf
	}

	log.Fatalf("Default context not found in kubeconfig")

	return nil
}

func loadServiceAccount() (*serverContextFields, error) {
	token, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, err
	}

	serverCA, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	pemBlock, _ := pem.Decode(serverCA)
	serverCert, err := x509.ParseCertificate(pemBlock.Bytes)
	if err != nil {
		return nil, err
	}

	servicePort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(servicePort) == 0 {
		return nil, fmt.Errorf("Unable to locate API server from KUBERNETES_SERVICE_PORT environment variable")
	}
	serviceHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	if len(serviceHost) == 0 {
		return nil, fmt.Errorf("Unable to locate API server from KUBERNETES_SERVICE_HOST environment variable")
	}

	return &serverContextFields{
		username:  "ServiceAccount",
		serverURL: "https://" + serviceHost + ":" + servicePort,
		serverCA:  serverCert,
		token:     string(token),
		insecure:  true,
	}, nil
}

func loadCert() []byte {
	cert, err := ioutil.ReadFile(*caCertFile)
	if err == nil {
		return cert
	}
	if config.CACert64 == nil {
		log.Fatal("Unable to load CA certificate from file or from config")
	}
	cert, err = base64.StdEncoding.DecodeString(*config.CACert64)
	if err != nil {
		log.Fatal("Unable to decode CA cert base64 from config")
	}
	return cert
}

func loadSecurity() *serverContextFields {
	yamlString, err := os.Open(*kubeConfigFilename)
	if err == nil {
		kconfig, err := kubeconfig.ReadKubeConfig(yamlString)
		if err != nil {
			log.Fatalf("Unable to read kubeconfig: %v", err)
		}
		return serverContextFromKubeconfig(kconfig)
	}
	sa, err := loadServiceAccount()
	if err != nil {
		log.Fatalf("No kubeconfig and no Kubernetes account found: %v", err)
	}
	return sa
}

func updateServerContextTicker(sa *serverContext) {
	for {
		saf := loadSecurity()
		sa.Lock()
		if !sa.f.isSameAs(saf) {
			log.Printf("Updating security context for API calls to Kubernetes")
			sa.f = *saf
		}
		sa.Unlock()
		time.Sleep(time.Second * 600)
	}
}

func main() {
	flag.Parse()

	c, err := LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}
	config = c
	config.DumpConfig()

	// load client cert/key, cacert
	clcert, err := tls.LoadX509KeyPair(*agentCertFile, *agentKeyFile)
	if err != nil {
		log.Fatalf("Unable to load agent certificate or key: %v", err)
	}
	caCertPool := x509.NewCertPool()
	srvcert := loadCert()
	if ok := caCertPool.AppendCertsFromPEM(srvcert); !ok {
		log.Fatalf("Unable to append certificate to pool: %v", err)
	}

	ta := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clcert},
		RootCAs:      caCertPool,
	})

	// First, try to see if we have a kubeconfig.yaml
	saf := loadSecurity()
	sa := &serverContext{f: *saf}

	go updateServerContextTicker(sa)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(ta),
		grpc.WithBlock(),
		grpc.WithTimeout(10 * time.Second),
	}

	conn, err := grpc.Dial(config.ControllerHostname, opts...)
	if err != nil {
		log.Fatalf("Could not connect: %v", err)
	}
	defer conn.Close()

	var wg sync.WaitGroup

	log.Printf("Starting Kubernetes tunnel.")
	wg.Add(1)
	go runKubernetesTunnel(&wg, sa, conn)

	wg.Wait()
	log.Printf("Done.")
}
