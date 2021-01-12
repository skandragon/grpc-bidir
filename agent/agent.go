package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	b64 "encoding/base64"
	"encoding/pem"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/opsmx/grpc-bidir/kubeconfig"
	"github.com/opsmx/grpc-bidir/tunnel"
)

var (
	tickTime           = flag.Int("tickTime", 30, "Time between sending Ping messages")
	host               = flag.String("host", tunnel.DefaultHostAndPort, "The address:port of the controller to connect to")
	agentCertFile      = flag.String("certFile", "/app/secrets/agent/tls.crt", "The file containing the certificate used to connect to the controller")
	agentKeyFile       = flag.String("keyFile", "/app/secrets/agent/tls.key", "The file containing the certificate used to connect to the controller")
	caCertFile         = flag.String("caCertFile", "/app/config/ca.pem", "The file containing the CA certificate we will use to verify the controller's cert")
	kubeConfigFilename = flag.String("kubeconfig", "/app/config/kubeconfig.yaml", "The location of a kubeconfig file to define endpoints and kube API auth")
	configFile         = flag.String("configFile", "/app/config/config.yaml", "The file with the controller config")

	config *AgentConfig
)

func makeHeaders(headers map[string][]string) []*tunnel.HttpHeader {
	ret := make([]*tunnel.HttpHeader, 0)
	for name, values := range headers {
		ret = append(ret, &tunnel.HttpHeader{Name: name, Values: values})
	}
	return ret
}

type cancelState struct {
	id     string
	cancel context.CancelFunc
}

var cancelRegistry = struct {
	sync.Mutex
	m map[string]context.CancelFunc
}{m: make(map[string]context.CancelFunc)}

func registerCancelFunction(id string, cancel context.CancelFunc) {
	cancelRegistry.Lock()
	cancelRegistry.m[id] = cancel
	cancelRegistry.Unlock()
}

func unregisterCancelFunction(id string) {
	cancelRegistry.Lock()
	delete(cancelRegistry.m, id)
	cancelRegistry.Unlock()
}

func callCancelFunction(id string) {
	cancelRegistry.Lock()
	cancel, ok := cancelRegistry.m[id]
	if ok {
		cancel()
		log.Printf("Cancelling request %s", id)
	}
	cancelRegistry.Unlock()
}

func executeRequest(dataflow chan *tunnel.ASEventWrapper, c *serverContext, req *tunnel.HttpRequest) {
	// TODO: A ServerCA is technically optional, but we might want to fail if it's not present...
	log.Printf("Running request %v", req)
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.insecure,
	}
	if c.serverCA != nil {
		caCertPool := x509.NewCertPool()
		caCertPool.AddCert(c.serverCA)
		tlsConfig.RootCAs = caCertPool
		tlsConfig.BuildNameToCertificate()
	}
	if c.clientCert != nil {
		tlsConfig.Certificates = []tls.Certificate{*c.clientCert}
	}
	tr := &http.Transport{
		MaxIdleConns:       10,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
		TLSClientConfig:    tlsConfig,
	}
	client := &http.Client{
		Transport: tr,
	}

	ctx, cancel := context.WithCancel(context.Background())

	registerCancelFunction(req.Id, cancel)
	defer func() {
		unregisterCancelFunction(req.Id)
	}()

	httpRequest, err := http.NewRequestWithContext(ctx, req.Method, c.serverURL+req.URI, bytes.NewBuffer(req.Body))
	if err != nil {
		log.Printf("Failed to build request for %s to %s: %v", req.Method, c.serverURL+req.URI, err)
		resp := &tunnel.ASEventWrapper{
			Event: &tunnel.ASEventWrapper_HttpResponse{
				HttpResponse: &tunnel.HttpResponse{
					Id:            req.Id,
					Target:        req.Target,
					Status:        http.StatusBadGateway,
					ContentLength: 0,
				},
			},
		}
		dataflow <- resp
		return
	}
	for _, header := range req.Headers {
		for _, value := range header.Values {
			httpRequest.Header.Add(header.Name, value)
		}
	}
	if len(c.token) > 0 {
		httpRequest.Header.Set("Authorization", "Bearer "+c.token)
	}
	log.Printf("Sending HTTP request: %v", httpRequest)
	get, err := client.Do(httpRequest)
	if err != nil {
		log.Printf("Failed to execute request for %s to %s: %v", req.Method, c.serverURL+req.URI, err)
		resp := &tunnel.ASEventWrapper{
			Event: &tunnel.ASEventWrapper_HttpResponse{
				HttpResponse: &tunnel.HttpResponse{
					Id:            req.Id,
					Target:        req.Target,
					Status:        http.StatusBadGateway,
					ContentLength: 0,
				},
			},
		}
		dataflow <- resp
		return
	}

	// First, send the headers.
	resp := &tunnel.ASEventWrapper{
		Event: &tunnel.ASEventWrapper_HttpResponse{
			HttpResponse: &tunnel.HttpResponse{
				Id:            req.Id,
				Target:        req.Target,
				Status:        int32(get.StatusCode),
				ContentLength: get.ContentLength,
				Headers:       makeHeaders(get.Header),
			},
		},
	}
	dataflow <- resp

	// Now, send one or more data packet.
	for {
		buf := make([]byte, 10240)
		n, err := get.Body.Read(buf)
		if n > 0 {
			resp := &tunnel.ASEventWrapper{
				Event: &tunnel.ASEventWrapper_HttpChunkedResponse{
					HttpChunkedResponse: &tunnel.HttpChunkedResponse{
						Id:     req.Id,
						Target: req.Target,
						Body:   buf[:n],
					},
				},
			}
			dataflow <- resp
		}
		if err == io.EOF {
			resp := &tunnel.ASEventWrapper{
				Event: &tunnel.ASEventWrapper_HttpChunkedResponse{
					HttpChunkedResponse: &tunnel.HttpChunkedResponse{
						Id:     req.Id,
						Target: req.Target,
						Body:   []byte(""),
					},
				},
			}
			dataflow <- resp
			return
		}
		if err == context.Canceled {
			log.Printf("Context cancelled, request ID %s", req.Id)
			return
		}
		if err != nil {
			log.Printf("Got error on HTTP read: %v", err)
			// todo: send an error message somehow.  For now, just send EOF
			resp := &tunnel.ASEventWrapper{
				Event: &tunnel.ASEventWrapper_HttpChunkedResponse{
					HttpChunkedResponse: &tunnel.HttpChunkedResponse{
						Id:     req.Id,
						Target: req.Target,
						Body:   []byte(""),
					},
				},
			}
			dataflow <- resp
			return
		}
	}
}

func runTunnel(sa *serverContext, client tunnel.TunnelServiceClient, ticker chan uint64, identity string) {
	ctx := context.Background()
	stream, err := client.EventTunnel(ctx)
	if err != nil {
		log.Fatalf("%v.EventTunnel(_) = _, %v", client, err)
	}
	hello := &tunnel.ASEventWrapper{
		Event: &tunnel.ASEventWrapper_AgentHello{
			AgentHello: &tunnel.AgentHello{
				Protocols:            []string{"kubernetes"},
				KubernetesNamespaces: config.Namespaces,
			},
		},
	}
	if err = stream.Send(hello); err != nil {
		log.Fatalf("Unable to send hello packet: %v", err)
	}

	dataflow := make(chan *tunnel.ASEventWrapper, 20)

	// Handle periodic pings from the ticker.
	go func() {
		for {
			ts, more := <-ticker
			if !more {
				return
			}
			req := &tunnel.ASEventWrapper{
				Event: &tunnel.ASEventWrapper_PingRequest{
					PingRequest: &tunnel.PingRequest{Ts: ts},
				},
			}
			if err = stream.Send(req); err != nil {
				log.Fatalf("Unable to send a PingRequest: %v", err)
			}
		}
	}()

	// Handle HTTP fetch responses
	go func() {
		for {
			ew, more := <-dataflow
			if !more {
				return
			}
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
			case *tunnel.SAEventWrapper_PingResponse:
				continue
			case *tunnel.SAEventWrapper_HttpRequestCancel:
				req := in.GetHttpRequestCancel()
				callCancelFunction(req.Id)
			case *tunnel.SAEventWrapper_HttpRequest:
				req := in.GetHttpRequest()
				go executeRequest(dataflow, sa, req)
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

func runTicker(tickTime int, ticker chan uint64) {
	log.Printf("Starting ticker to send pings every %d seconds.", tickTime)
	go func() {
		for {
			time.Sleep(time.Duration(tickTime) * time.Second)
			ticker <- tunnel.Now()
		}
	}()

}

type serverContext struct {
	username   string
	serverURL  string
	serverCA   *x509.Certificate
	clientCert *tls.Certificate
	token      string
	insecure   bool
}

func makeServerConfig(kconfig *kubeconfig.KubeConfig) *serverContext {
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

		sa := &serverContext{
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
			sa.serverCA = serverCert
		}

		return sa
	}

	log.Fatalf("Default context not found in kubeconfig")

	return nil
}

func loadServiceAccount() *serverContext {

	token, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		log.Printf("Unable to read service account token, falling back to kubectl config file")
		return nil
	}

	serverCA, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		log.Printf("Unable to read service account ca.crt, falling back to kubectl config file")
		return nil
	}
	pemBlock, _ := pem.Decode(serverCA)
	serverCert, err := x509.ParseCertificate(pemBlock.Bytes)
	if err != nil {
		log.Fatalf("Error parsing server certificate: %v", err)
	}

	servicePort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(servicePort) == 0 {
		log.Printf("Unable to locate API server from KUBERNETES_SERVICE_PORT environment variable")
		return nil
	}
	serviceHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	if len(serviceHost) == 0 {
		log.Printf("Unable to locate API server from KUBERNETES_SERVICE_HOST environment variable")
		return nil
	}

	log.Printf("Using this pod's ServiceAccount to authenticate to Kubernetes API")
	log.Printf("Server CA: %v", serverCert.PublicKey)

	sa := &serverContext{
		username:  "ServiceAccount",
		serverURL: "https://" + serviceHost + ":" + servicePort,
		serverCA:  serverCert,
		token:     string(token),
		insecure:  true,
	}
	return sa
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
	srvcert, err := ioutil.ReadFile(*caCertFile)
	if err != nil {
		log.Fatalf("Unable to load CA certificate: %v", err)
	}
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(srvcert); !ok {
		log.Fatalf("Unable to append certificate to pool: %v", err)
	}

	ta := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clcert},
		RootCAs:      caCertPool,
	})

	// First, try to see if we have a kubeconfig.yaml
	var sa *serverContext
	yamlString, err := os.Open(*kubeConfigFilename)
	if err != nil {
		sa = loadServiceAccount()
		if sa == nil {
			log.Fatalf("No kubeconfig and no Kubernetes account found")
		}
	} else {
		kconfig, err := kubeconfig.ReadKubeConfig(yamlString)
		if err != nil {
			log.Fatalf("Unable to read kubeconfig: %v", err)
		}
		sa = makeServerConfig(kconfig)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(ta),
		grpc.WithBlock(),
		grpc.WithTimeout(10 * time.Second),
	}

	conn, err := grpc.Dial(*host, opts...)
	if err != nil {
		log.Fatalf("Could not connect: %v", err)
	}
	defer conn.Close()

	client := tunnel.NewTunnelServiceClient(conn)

	ticker := make(chan uint64)
	runTicker(*tickTime, ticker)

	log.Printf("Starting tunnel.")
	runTunnel(sa, client, ticker, "skan")
	log.Printf("Done.")
}
