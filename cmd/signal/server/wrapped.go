package server

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	log "github.com/pion/ion-log"
	"github.com/soheilhy/cmux"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

type WrapperedServerOptions struct {
	Addr                  string
	Cert                  string
	Key                   string
	AllowAllOrigins       bool
	AutoTLS               bool
	Domain                string
	AllowedOrigins        *[]string
	AllowedHeaders        *[]string
	UseWebSocket          bool
	WebsocketPingInterval time.Duration
}

func DefaultWrapperedServerOptions() WrapperedServerOptions {
	return WrapperedServerOptions{
		Addr:                  ":9090",
		Cert:                  "",
		Key:                   "",
		AllowAllOrigins:       true,
		AllowedHeaders:        &[]string{},
		AllowedOrigins:        &[]string{},
		UseWebSocket:          true,
		WebsocketPingInterval: 0,
	}
}

type WrapperedGRPCWebServer struct {
	options    WrapperedServerOptions
	GRPCServer *grpc.Server
}

func NewWrapperedGRPCWebServer(options WrapperedServerOptions, s *grpc.Server) *WrapperedGRPCWebServer {
	return &WrapperedGRPCWebServer{
		options:    options,
		GRPCServer: s,
	}
}

type allowedOrigins struct {
	origins map[string]struct{}
}

func (a *allowedOrigins) IsAllowed(origin string) bool {
	_, ok := a.origins[origin]
	return ok
}

func makeAllowedOrigins(origins []string) *allowedOrigins {
	o := map[string]struct{}{}
	for _, allowedOrigin := range origins {
		o[allowedOrigin] = struct{}{}
	}
	return &allowedOrigins{
		origins: o,
	}
}

func (s *WrapperedGRPCWebServer) makeHTTPOriginFunc(allowedOrigins *allowedOrigins) func(origin string) bool {
	if s.options.AllowAllOrigins {
		return func(origin string) bool {
			return true
		}
	}
	return allowedOrigins.IsAllowed
}

func (s *WrapperedGRPCWebServer) makeWebsocketOriginFunc(allowedOrigins *allowedOrigins) func(req *http.Request) bool {
	if s.options.AllowAllOrigins {
		return func(req *http.Request) bool {
			return true
		}
	}
	return func(req *http.Request) bool {
		origin, err := grpcweb.WebsocketRequestOrigin(req)
		if err != nil {
			log.Warnf("%v", err)
			return false
		}
		return allowedOrigins.IsAllowed(origin)
	}
}

func (s *WrapperedGRPCWebServer) Serve() error {
	addr := s.options.Addr

	if s.options.AllowAllOrigins && s.options.AllowedOrigins != nil && len(*s.options.AllowedOrigins) != 0 {
		log.Errorf("Ambiguous --allow_all_origins and --allow_origins configuration. Either set --allow_all_origins=true OR specify one or more origins to whitelist with --allow_origins, not both.")
	}

	allowedOrigins := makeAllowedOrigins(*s.options.AllowedOrigins)

	options := []grpcweb.Option{
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
		grpcweb.WithOriginFunc(s.makeHTTPOriginFunc(allowedOrigins)),
	}

	if s.options.UseWebSocket {
		log.Infof("Using websockets")
		options = append(
			options,
			grpcweb.WithWebsockets(true),
			grpcweb.WithWebsocketOriginFunc(s.makeWebsocketOriginFunc(allowedOrigins)),
		)

		if s.options.WebsocketPingInterval >= time.Second {
			log.Infof("websocket keepalive pinging enabled, the timeout interval is %s", s.options.WebsocketPingInterval.String())
			options = append(
				options,
				grpcweb.WithWebsocketPingInterval(s.options.WebsocketPingInterval),
			)
		}
	}

	if s.options.AllowedHeaders != nil && len(*s.options.AllowedHeaders) > 0 {
		options = append(
			options,
			grpcweb.WithAllowedRequestHeaders(*s.options.AllowedHeaders),
		)
	}

	m := autocert.Manager{
		Prompt: autocert.AcceptTOS,
		// HostPolicy: autocert.HostWhitelist(*host),
		Cache: autocert.DirCache("/certs"),
	}

	wrappedServer := grpcweb.WrapServer(s.GRPCServer, options...)
	handler := func(resp http.ResponseWriter, req *http.Request) {
		wrappedServer.ServeHTTP(resp, req)
	}

	log.Infof("s.options.AutoTLS=%v s.options.Domain=%v", s.options.AutoTLS, s.options.Domain)

	httpServer := http.Server{
		Addr: addr,
		// Handler: http.HandlerFunc(handler),
		Handler: m.HTTPHandler(http.HandlerFunc(handler)),
	}

	httpServer.TLSConfig = m.TLSConfig()

	// if s.options.AutoTLS && s.options.Domain != "" {
	// log.Infof("autotls.Run!")
	// // err := autotls.Run(httpServer.Handler, s.options.Domain)
	// // Run support 1-line LetsEncrypt HTTPS servers
	// err := http.Serve(autocert.NewListener(s.options.Domain), httpServer.Handler)
	// log.Infof("autotls.Run!")
	// if err != nil {
	// log.Errorf("autotls.Run err=%v", err)
	// return err
	// }
	// log.Infof("autotls.Run on %v", s.options.Domain)
	// }
	var listener net.Listener

	enableTLS := s.options.Cert != "" && s.options.Key != ""

	if enableTLS {
		log.Infof("enableTLS!")
		cer, err := tls.LoadX509KeyPair(s.options.Cert, s.options.Key)
		if err != nil {
			log.Panicf("failed to load x509 key pair: %v", err)
			return err
		}
		config := &tls.Config{Certificates: []tls.Certificate{cer}}
		tls, err := tls.Listen("tcp", addr, config)
		if err != nil {
			log.Panicf("failed to listen: tls %v", err)
			return err
		}
		listener = tls
	} else {
		tcp, err := net.Listen("tcp", addr)
		if err != nil {
			log.Panicf("failed to listen: tcp %v", err)
			return err
		}
		listener = tcp
	}

	log.Infof("Starting grpc/grpc-web server, bind: %s, with TLS: %v", addr, enableTLS)

	mux := cmux.New(listener)
	grpcListener := mux.Match(cmux.HTTP2())
	httpListener := mux.Match(cmux.HTTP1Fast())
	g := new(errgroup.Group)
	g.Go(func() error { return s.GRPCServer.Serve(grpcListener) })
	g.Go(func() error { return httpServer.Serve(httpListener) })
	g.Go(mux.Serve)
	log.Infof("Run server: %v", g.Wait())
	return nil
}
