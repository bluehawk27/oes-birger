/*
 * Copyright 2021 OpsMx, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License")
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package serviceconfig

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/opsmx/oes-birger/internal/kubeconfig"
	"github.com/opsmx/oes-birger/internal/tunnel"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v3"
)

type kubernetesConfig struct {
	KubeConfig string `yaml:"kubeConfig,omitempty"`
}

// KubernetesEndpoint implements a kubernetes endpoint state, including the credentials and namespaces
// defined in the configuration.
type KubernetesEndpoint struct {
	sync.RWMutex
	f      kubeContext
	config kubernetesConfig
}

type kubeContext struct {
	username   string
	serverURL  string
	serverCA   *x509.Certificate
	clientCert *tls.Certificate
	token      string
	insecure   bool
}

// MakeKubernetesEndpoint creates a new Kubernetes endpoint based on the provided config.
func MakeKubernetesEndpoint(name string, configBytes []byte) (*KubernetesEndpoint, bool, error) {
	k := &KubernetesEndpoint{}

	var config kubernetesConfig
	err := yaml.Unmarshal(configBytes, &config)
	if err != nil {
		return nil, false, err
	}

	if config.KubeConfig == "" {
		config.KubeConfig = "/app/config/kubeconfig.yaml"
	}

	k.config = config
	k.f = *k.loadKubernetesSecurity()

	go k.updateServerContextTicker()

	return k, true, nil
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
			zap.S().Fatalf("Unable to retrieve cluster and user info for context %s: %v", name, err)
		}

		certData, err := base64.StdEncoding.DecodeString(user.User.ClientCertificateData)
		if err != nil {
			zap.S().Fatalf("Error decoding user cert from base64 (%s): %v", user.Name, err)
		}
		keyData, err := base64.StdEncoding.DecodeString(user.User.ClientKeyData)
		if err != nil {
			zap.S().Fatalf("Error decoding user key from base64 (%s): %v", user.Name, err)
		}

		clientKeypair, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			zap.S().Fatalf("Error loading client cert/key: %v", err)
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
				zap.S().Fatalf("Error decoding server CA cert from base64 (%s): %v", cluster.Name, err)
			}
			pemBlock, _ := pem.Decode(serverCA)
			serverCert, err := x509.ParseCertificate(pemBlock.Bytes)
			if err != nil {
				zap.S().Fatalf("Error parsing server certificate: %v", err)
			}
			saf.serverCA = serverCert
		}

		return saf
	}

	zap.S().Fatalf("Default context not found in kubeconfig")

	return nil
}

func tlsCertEqual(s1 *tls.Certificate, s2 *tls.Certificate) bool {
	if s1 == nil && s2 == nil {
		return true
	}
	if (s1 != nil && s2 == nil) || (s1 == nil && s2 != nil) {
		return false
	}
	return bytes.Equal(s1.Certificate[0], s2.Certificate[0])
}

func x509CertEqual(s1 *x509.Certificate, s2 *x509.Certificate) bool {
	if s1 == nil && s2 == nil {
		return true
	}
	if (s1 != nil && s2 == nil) || (s1 == nil && s2 != nil) {
		return false
	}
	return s1.Equal(s2)
}

func (scf *kubeContext) isSameAs(scf2 *kubeContext) bool {
	if scf.username != scf2.username || scf.serverURL != scf2.serverURL || scf.token != scf2.token || scf.insecure != scf2.insecure {
		return false
	}

	if !x509CertEqual(scf.serverCA, scf2.serverCA) {
		return false
	}

	return tlsCertEqual(scf.clientCert, scf2.clientCert)
}

func (ke *KubernetesEndpoint) loadServiceAccount() (*kubeContext, error) {
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, err
	}

	serverCA, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
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

// ExecuteHTTPRequest does the actual call to connect to HTTP, and will send the data back over the
// tunnel.
func (ke *KubernetesEndpoint) ExecuteHTTPRequest(_ string, dataflow chan *tunnel.MessageWrapper, req *tunnel.OpenHTTPTunnelRequest) {
	c := ke.makeServerContextFields()

	// TODO: A ServerCA is technically optional, but we might want to fail if it's not present...
	zap.S().Debugw("running request", "request", "req")
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
	tunnel.RegisterCancelFunction(req.Id, cancel)
	defer tunnel.UnregisterCancelFunction(req.Id)

	httpRequest, err := http.NewRequestWithContext(ctx, req.Method, c.serverURL+req.URI, bytes.NewBuffer(req.Body))
	if err != nil {
		zap.S().Warnf("Failed to build request for %s to %s: %v", req.Method, c.serverURL+req.URI, err)
		dataflow <- tunnel.MakeBadGatewayResponse(req.Id)
		return
	}

	err = tunnel.CopyHeaders(req.Headers, &httpRequest.Header)
	if err != nil {
		zap.S().Warnf("failed to copy headers: %v", err)
		dataflow <- tunnel.MakeBadGatewayResponse(req.Id)
		return
	}
	if len(c.token) > 0 {
		httpRequest.Header.Set("Authorization", "Bearer "+c.token)
	}

	tunnel.RunHTTPRequest(client, req, httpRequest, dataflow, c.serverURL)
}

func (ke *KubernetesEndpoint) loadKubernetesSecurity() *kubeContext {
	yamlString, err := os.Open(ke.config.KubeConfig)
	if err == nil {
		kconfig, err := kubeconfig.ReadKubeConfig(yamlString)
		if err != nil {
			zap.S().Fatalf("Unable to read kubeconfig: %v", err)
		}
		return ke.serverContextFromKubeconfig(kconfig)
	}
	sa, err := ke.loadServiceAccount()
	if err != nil {
		zap.S().Fatalf("No kubeconfig and no Kubernetes account found: %v", err)
	}
	return sa
}

func (ke *KubernetesEndpoint) updateServerContextTicker() {
	for {
		saf := ke.loadKubernetesSecurity()
		ke.Lock()
		if !ke.f.isSameAs(saf) {
			zap.L().Info("Updating security context for API calls to Kubernetes")
			ke.f = *saf
		}
		ke.Unlock()
		time.Sleep(time.Second * 600)
	}
}
