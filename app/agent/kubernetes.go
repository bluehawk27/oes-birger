package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/opsmx/oes-birger/pkg/kubeconfig"
	"github.com/opsmx/oes-birger/pkg/tunnel"
	"golang.org/x/net/context"
)

type KubernetesEndpoint struct {
	sync.RWMutex
	f kubeContext
}

type kubeContext struct {
	username   string
	serverURL  string
	serverCA   *x509.Certificate
	clientCert *tls.Certificate
	token      string
	insecure   bool
}

func MakeKubernetesEndpoint() *KubernetesEndpoint {
	k := &KubernetesEndpoint{}
	k.f = *k.loadKubernetesSecurity()
	go k.updateServerContextTicker()
	return k
}

func (ke *KubernetesEndpoint) makeServerContextFields() *kubeContext {
	ke.RLock()
	defer ke.RUnlock()
	return &kubeContext{
		username:   ke.f.username,
		serverURL:  ke.f.serverURL,
		serverCA:   ke.f.serverCA,
		clientCert: ke.f.clientCert,
		token:      ke.f.token,
		insecure:   ke.f.insecure,
	}
}

func (ke *KubernetesEndpoint) serverContextFromKubeconfig(kconfig *kubeconfig.KubeConfig) *kubeContext {
	names := kconfig.GetContextNames()
	for _, name := range names {
		if name != kconfig.CurrentContext {
			continue
		}
		user, cluster, err := kconfig.FindContext(name)
		if err != nil {
			log.Fatalf("Unable to retrieve cluster and user info for context %s: %v", name, err)
		}

		certData, err := base64.StdEncoding.DecodeString(user.User.ClientCertificateData)
		if err != nil {
			log.Fatalf("Error decoding user cert from base64 (%s): %v", user.Name, err)
		}
		keyData, err := base64.StdEncoding.DecodeString(user.User.ClientKeyData)
		if err != nil {
			log.Fatalf("Error decoding user key from base64 (%s): %v", user.Name, err)
		}

		clientKeypair, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			log.Fatalf("Error loading client cert/key: %v", err)
		}

		saf := &kubeContext{
			username:   user.Name,
			clientCert: &clientKeypair,
			serverURL:  cluster.Cluster.Server,
			insecure:   cluster.Cluster.InsecureSkipTLSVerify,
		}

		if len(cluster.Cluster.CertificateAuthorityData) > 0 {
			serverCA, err := base64.StdEncoding.DecodeString(cluster.Cluster.CertificateAuthorityData)
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

func (scf *kubeContext) isSameAs(scf2 *kubeContext) bool {
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
		if !bytes.Equal(scf.clientCert.Certificate[0], scf2.clientCert.Certificate[0]) {
			return false
		}
	}

	return true
}

func (ke *KubernetesEndpoint) loadServiceAccount() (*kubeContext, error) {
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
		return nil, fmt.Errorf("unable to locate API server from KUBERNETES_SERVICE_PORT environment variable")
	}
	serviceHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	if len(serviceHost) == 0 {
		return nil, fmt.Errorf("unable to locate API server from KUBERNETES_SERVICE_HOST environment variable")
	}

	return &kubeContext{
		username:  "ServiceAccount",
		serverURL: "https://" + serviceHost + ":" + servicePort,
		serverCA:  serverCert,
		token:     string(token),
		insecure:  true,
	}, nil
}

func (ke *KubernetesEndpoint) executeHTTPRequest(dataflow chan *tunnel.AgentToControllerWrapper, req *tunnel.HttpRequest) {
	c := ke.makeServerContextFields()

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
		//tlsConfig.BuildNameToCertificate()
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
	defer unregisterCancelFunction(req.Id)

	httpRequest, err := http.NewRequestWithContext(ctx, req.Method, c.serverURL+req.URI, bytes.NewBuffer(req.Body))
	if err != nil {
		log.Printf("Failed to build request for %s to %s: %v", req.Method, c.serverURL+req.URI, err)
		dataflow <- makeBadGatewayResponse(req.Id)
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
	log.Printf("Sending HTTP request: %s to %v", req.Method, c.serverURL+req.URI)
	get, err := client.Do(httpRequest)
	if err != nil {
		log.Printf("Failed to execute request for %s to %s: %v", req.Method, c.serverURL+req.URI, err)
		dataflow <- makeBadGatewayResponse(req.Id)
		return
	}

	// First, send the headers.
	resp := makeResponse(req.Id, get)
	dataflow <- resp

	// Now, send one or more data packet.
	for {
		buf := make([]byte, 10240)
		n, err := get.Body.Read(buf)
		if n > 0 {
			resp := makeChunkedResponse(req.Id, buf[:n])
			dataflow <- resp
		}
		if err == io.EOF {
			resp := makeChunkedResponse(req.Id, emptyBytes)
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
			resp := makeChunkedResponse(req.Id, emptyBytes)
			dataflow <- resp
			return
		}
	}
}

func (ke *KubernetesEndpoint) loadKubernetesSecurity() *kubeContext {
	yamlString, err := os.Open(config.Kubernetes.KubeConfig)
	if err == nil {
		kconfig, err := kubeconfig.ReadKubeConfig(yamlString)
		if err != nil {
			log.Fatalf("Unable to read kubeconfig: %v", err)
		}
		return ke.serverContextFromKubeconfig(kconfig)
	}
	sa, err := ke.loadServiceAccount()
	if err != nil {
		log.Fatalf("No kubeconfig and no Kubernetes account found: %v", err)
	}
	return sa
}

func (ke *KubernetesEndpoint) updateServerContextTicker() {
	for {
		saf := ke.loadKubernetesSecurity()
		ke.Lock()
		if !ke.f.isSameAs(saf) {
			log.Printf("Updating security context for API calls to Kubernetes")
			ke.f = *saf
		}
		ke.Unlock()
		time.Sleep(time.Second * 600)
	}
}
