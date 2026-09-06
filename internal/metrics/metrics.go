package metrics

import (
	"context"
	"net/http"
	_ "net/http/pprof" // Registers pprof handlers into DefaultServeMux
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	AgentsOnline = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rdprelay_agent_online",
		Help: "Current number of online agents connected via QUIC",
	})

	ActiveTCPConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rdprelay_tcp_connections",
		Help: "Current number of active RDP TCP proxy connections",
	})

	ActiveUDPSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rdprelay_udp_sessions",
		Help: "Current number of active UDP sessions",
	})

	TCPBytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rdprelay_tcp_bytes_total",
		Help: "Total RDP TCP bytes transferred",
	}, []string{"direction"})

	UDPBytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rdprelay_udp_bytes_total",
		Help: "Total RDP UDP bytes transferred",
	}, []string{"direction"})

	UDPPacketsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rdprelay_udp_packets_total",
		Help: "Total RDP UDP packets transferred",
	}, []string{"direction"})

	UDPDatagramDropTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rdprelay_udp_datagram_drop_total",
		Help: "Total UDP datagrams dropped due to error, corruption or ACL",
	})

	UDPFragmentCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rdprelay_udp_fragment_created_total",
		Help: "Total UDP fragments generated",
	})

	UDPFragmentTimeoutTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rdprelay_udp_fragment_timeout_total",
		Help: "Total UDP reassembly fragment timeouts",
	})

	UDPSessionCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rdprelay_udp_session_created_total",
		Help: "Total UDP sessions created",
	})

	UDPSessionExpiredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rdprelay_udp_session_expired_total",
		Help: "Total UDP sessions expired and cleaned",
	})
)

func init() {
	prometheus.MustRegister(
		AgentsOnline,
		ActiveTCPConnections,
		ActiveUDPSessions,
		TCPBytesTotal,
		UDPBytesTotal,
		UDPPacketsTotal,
		UDPDatagramDropTotal,
		UDPFragmentCreatedTotal,
		UDPFragmentTimeoutTotal,
		UDPSessionCreatedTotal,
		UDPSessionExpiredTotal,
	)
}

// Server provides HTTP endpoint for Prometheus metrics and Go pprof
type Server struct {
	httpServer *http.Server
}

// StartServer starts Prometheus metrics and pprof server
func StartServer(addr string) *Server {
	if addr == "" {
		return nil
	}

	mux := http.DefaultServeMux
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		_ = srv.ListenAndServe()
	}()

	return &Server{httpServer: srv}
}

// Shutdown gracefully shuts down the metrics server
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}
