package main

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

import (
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/OpsMx/go-app-base/tracer"
	"github.com/OpsMx/go-app-base/util"
	"github.com/OpsMx/go-app-base/version"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/opsmx/oes-birger/app/forwarder-controller/cncserver"
	"github.com/opsmx/oes-birger/internal/ca"
	"github.com/opsmx/oes-birger/internal/jwtutil"
	"github.com/opsmx/oes-birger/internal/secrets"
	"github.com/opsmx/oes-birger/internal/serviceconfig"
	"github.com/opsmx/oes-birger/internal/tunnelroute"
	"github.com/opsmx/oes-birger/internal/webhook"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	appName = "forwarder-controller"
)

var (
	configFile = flag.String("configFile", "/app/config/config.yaml", "The file with the controller config")

	// eg, http://localhost:14268/api/traces
	jaegerEndpoint = flag.String("jaeger-endpoint", "", "Jaeger collector endpoint")
	traceToStdout  = flag.Bool("traceToStdout", false, "log traces to stdout")
	traceRatio     = flag.Float64("traceRatio", 0.01, "ratio of traces to create, if incoming request is not traced")
	showversion    = flag.Bool("version", false, "show the version and exit")

	tracerProvider *tracer.TracerProvider

	jwtKeyset     = jwk.NewSet()
	jwtCurrentKey string
	config        *ControllerConfig
	secretsLoader secrets.SecretLoader
	authority     *ca.CA
	hook          *webhook.Runner
	routes        = tunnelroute.MakeRoutes()
	endpoints     []serviceconfig.ConfiguredEndpoint
	logger        *zap.Logger
	sl            *zap.SugaredLogger
)

func getAgentNameFromContext(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "no peer found")
	}
	tlsAuth, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "unexpected peer transport credentials")
	}
	if len(tlsAuth.State.VerifiedChains) == 0 || len(tlsAuth.State.VerifiedChains[0]) == 0 {
		return "", status.Error(codes.Unauthenticated, "could not verify peer certificate")
	}
	return getAgentNameFromCertificate(tlsAuth.State.VerifiedChains[0][0])
}

func getAgentNameFromCertificate(cert *x509.Certificate) (string, error) {
	// TODO: should verify the certificate here...
	names, err := ca.GetCertificateNameFromCert(cert)
	if err != nil {
		return "", err
	}
	if names.Purpose != ca.CertificatePurposeAgent {
		return "", fmt.Errorf("not an agent certificate")
	}
	return names.Agent, nil
}

//
// Flow:
//  * API request comes in
//  * We look in our local list of possible endpoints.  Error if not found.
//  * One of the endpoint paths (directly connected preferred, but if none use another controller)
//  * The message is sent to the endpoint.
//  * If the "other side" cancels the request, we expect to get notified.
//  * If we cancel the request, we notify the endpoint.
//  * Multiple data packets can flow in either direction:  { header, data... }
//  * If the endpoint vanishes, we will cancel all outstanding transactions.

// Impl:
//
// An agent uses a tunnel, which will allow messages to flow back and forth. If the connection
// is closed, we can detect this.  Each agent is known by a name ("Target")
// and one protocol it can handle.
//
// A peer controller also uses a tunnel, where it sends a list of ( protocol, agentID, agentSession )
// to allow proxying through this controller.  If it closes, all endpoints handled by this
// tunnel are closed.
//

func healthcheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	n, err := w.Write([]byte("{}"))
	if err != nil {
		log.Printf("Error writing healthcheck response: %v", err)
		return
	}
	if n != 2 {
		log.Printf("Failed to write 2 bytes: %d written", n)
	}
}

func runPrometheusHTTPServer(port uint16) {
	log.Printf("Running HTTP listener for Prometheus on port %d", port)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/", healthcheck)
	mux.HandleFunc("/health", healthcheck)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	log.Fatal(server.ListenAndServe())
}

func loadKeyset() {
	if config.ServiceAuth.CurrentKeyName == "" {
		log.Fatalf("No primary serviceAuth key name provided")
	}

	err := filepath.WalkDir(config.ServiceAuth.SecretsPath, func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// skip not refular files
		if !info.Type().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		key, err := jwk.New(content)
		if err != nil {
			return err
		}
		err = key.Set(jwk.KeyIDKey, info.Name())
		if err != nil {
			return err
		}
		err = key.Set(jwk.AlgorithmKey, jwa.HS256)
		if err != nil {
			return err
		}
		jwtKeyset.Add(key)
		log.Printf("Loaded service key name %s, length %d", info.Name(), len(content))
		return nil
	})
	if err != nil {
		log.Fatalf("cannot load key serviceAuth keys: %v", err)
	}

	jwtCurrentKey = config.ServiceAuth.CurrentKeyName
	if len(jwtCurrentKey) == 0 {
		log.Fatal("serviceAuth.currentKeyName is not set")
	}
	if _, found := jwtKeyset.LookupKeyID(jwtCurrentKey); !found {
		log.Fatal("serviceAuth.currentKeyName is not in the loaded list of keys")
	}

	if len(config.ServiceAuth.HeaderMutationKeyName) == 0 {
		log.Fatal("serviceAuth.headerMutationKeyName is not set")
	}

	log.Printf("Loaded %d serviceKeys", jwtKeyset.Len())
}

func parseConfig(filename string) (*ControllerConfig, error) {
	f, err := os.Open(*configFile)
	if err != nil {
		return nil, fmt.Errorf("while opening configfile: %w", err)
	}

	c, err := LoadConfig(f)
	if err != nil {
		return nil, fmt.Errorf("while loading config: %w", err)
	}

	return c, nil
}

func main() {
	log.Printf("%s", version.VersionString())
	flag.Parse()
	if *showversion {
		os.Exit(0)
	}

	var err error

	logger, err = zap.NewProduction()
	if err != nil {
		log.Fatalf("setting up logger: %v", err)
	}
	defer func() {
		_ = logger.Sync()
	}()
	_ = zap.ReplaceGlobals(logger)
	sl = logger.Sugar()

	sl.Infow("controller starting",
		"version", version.VersionString(),
		"os", runtime.GOOS,
		"arch", runtime.GOARCH,
		"cores", runtime.NumCPU(),
	)

	grpc.EnableTracing = true

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, syscall.SIGTERM, syscall.SIGINT)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *jaegerEndpoint != "" {
		*jaegerEndpoint = util.GetEnvar("JAEGER_TRACE_URL", "")
	}

	tracerProvider, err = tracer.NewTracerProvider(*jaegerEndpoint, *traceToStdout, version.GitHash(), appName, *traceRatio)
	util.Check(err)
	defer tracerProvider.Shutdown(ctx)

	config, err = parseConfig(*configFile)
	if err != nil {
		log.Fatalf("%v", err)
	}
	config.Dump()

	namespace, ok := os.LookupEnv("POD_NAMESPACE")
	if ok {
		secretsLoader, err = secrets.MakeKubernetesSecretLoader(namespace)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		log.Printf("POD_NAMESPACE not set.  Disabling Kubeernetes secret handling.")
	}

	loadKeyset()

	// Create registry entries to sign and validate JWTs for service authentication,
	// and protect x-spinnaker-user header.
	if err = jwtutil.RegisterServiceauthKeyset(jwtKeyset, config.ServiceAuth.CurrentKeyName); err != nil {
		log.Fatal(err)
	}
	if err = jwtutil.RegisterMutationKeyset(jwtKeyset, config.ServiceAuth.HeaderMutationKeyName); err != nil {
		log.Fatal(err)
	}

	if len(config.Webhook) > 0 {
		hook = webhook.NewRunner(config.Webhook)
		go hook.Run()
	}

	//
	// Make a new CA, for our use to generate server and other certificates.
	//
	caLocal, err := ca.LoadCAFromFile(config.CAConfig)
	if err != nil {
		log.Fatalf("Cannot create authority: %v", err)
	}
	authority = caLocal

	//
	// Make a server certificate.
	//
	log.Println("Generating a server certificate...")
	serverCert, err := authority.MakeServerCert(config.ServerNames)
	if err != nil {
		log.Fatalf("Cannot make server certificate: %v", err)
	}

	endpoints = serviceconfig.ConfigureEndpoints(secretsLoader, &config.ServiceConfig)

	cnc := cncserver.MakeCNCServer(config, authority, routes, version.GitBranch())
	go cnc.RunServer(*serverCert)

	go runAgentGRPCServer(config.InsecureAgentConnections, *serverCert)

	// Always listen on our well-known port, and always use HTTPS for this one.
	go serviceconfig.RunHTTPSServer(routes, authority, *serverCert, serviceconfig.IncomingServiceConfig{
		Name: "_services",
		Port: config.ServiceListenPort,
	})

	// Now, add all the others defined by our config.
	for _, service := range config.ServiceConfig.IncomingServices {
		if service.UseHTTP {
			go serviceconfig.RunHTTPServer(routes, service)
		} else {
			go serviceconfig.RunHTTPSServer(routes, authority, *serverCert, service)
		}
	}

	go runPrometheusHTTPServer(config.PrometheusListenPort)

	<-sigchan
	log.Printf("Exiting Cleanly")
}
