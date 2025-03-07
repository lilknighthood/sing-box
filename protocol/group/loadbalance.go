package group

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/interrupt"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/group/balancer"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

func RegisterLoadBalance(registry *outbound.Registry) {
	outbound.Register[option.LoadBalanceOutboundOptions](registry, C.TypeLoadBalance, NewLoadBalance)
}

var (
	_ adapter.Outbound                = (*LoadBalance)(nil)
	_ adapter.OutboundCheckGroup      = (*LoadBalance)(nil)
	_ adapter.Service                 = (*LoadBalance)(nil)
	_ adapter.InterfaceUpdateListener = (*LoadBalance)(nil)
)

// LoadBalance is a load balance group
type LoadBalance struct {
	outbound.GroupAdapter
	*balancer.Balancer

	ctx        context.Context
	router     adapter.Router
	logger     log.ContextLogger
	outbound   adapter.OutboundManager
	provider   adapter.ProviderManager
	connection adapter.ConnectionManager
	options    option.LoadBalanceOutboundOptions
}

// NewLoadBalance creates a new load balance outbound
func NewLoadBalance(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.LoadBalanceOutboundOptions) (adapter.Outbound, error) {
	return &LoadBalance{
		GroupAdapter: outbound.NewGroupAdapter(C.TypeLoadBalance, tag, []string{N.NetworkTCP, N.NetworkUDP}, router, options.ProviderGroupCommonOption),
		ctx:          ctx,
		router:       router,
		logger:       logger,
		outbound:     service.FromContext[adapter.OutboundManager](ctx),
		provider:     service.FromContext[adapter.ProviderManager](ctx),
		connection:   service.FromContext[adapter.ConnectionManager](ctx),
		options:      options,
	}, nil
}

// Now implements adapter.OutboundGroup
func (s *LoadBalance) Now() string {
	picked := s.Pick(context.Background(), N.NetworkTCP, M.Socksaddr{})
	if picked == nil {
		return ""
	}
	return picked.Tag()
}

// All implements adapter.OutboundGroup
func (s *LoadBalance) All() []string {
	s.LogNodes()
	return s.GroupAdapter.All()
}

// Network implements adapter.OutboundGroup
func (s *LoadBalance) Network() []string {
	return s.Balancer.Networks()
}

// DialContext implements adapter.Outbound
func (s *LoadBalance) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	var lastErr error
	maxRetry := 5
	for i := 0; i < maxRetry; i++ {
		picked := s.Pick(ctx, network, destination)
		if picked == nil {
			lastErr = E.New("no outbound available")
			break
		}
		conn, err := picked.DialContext(ctx, network, destination)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		s.logger.ErrorContext(ctx, err)
		s.ReportFailure(picked)
	}
	return nil, lastErr
}

// ListenPacket implements adapter.Outbound
func (s *LoadBalance) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	var lastErr error
	maxRetry := 5
	for i := 0; i < maxRetry; i++ {
		picked := s.Pick(ctx, N.NetworkUDP, destination)
		if picked == nil {
			lastErr = E.New("no outbound available")
			break
		}
		conn, err := picked.ListenPacket(ctx, destination)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		s.logger.ErrorContext(ctx, err)
		s.ReportFailure(picked)
	}
	return nil, lastErr
}

// NewConnectionEx implements adapter.TCPInjectableInbound
func (s *LoadBalance) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	selected := s.Pick(ctx, N.NetworkUDP, metadata.Destination)
	if selected == nil {
		s.connection.NewConnection(ctx, newErrDailer(E.New("no outbound available")), conn, metadata, onClose)
		return
	}
	ctx = interrupt.ContextWithIsExternalConnection(ctx)
	if outboundHandler, isHandler := selected.(adapter.ConnectionHandlerEx); isHandler {
		outboundHandler.NewConnectionEx(ctx, conn, metadata, onClose)
	} else {
		s.connection.NewConnection(ctx, selected, conn, metadata, onClose)
	}
}

// NewPacketConnectionEx implements adapter.UDPInjectableInbound
func (s *LoadBalance) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	selected := s.Pick(ctx, N.NetworkUDP, metadata.Destination)
	if selected == nil {
		s.connection.NewPacketConnection(ctx, newErrDailer(E.New("no outbound available")), conn, metadata, onClose)
		return
	}
	ctx = interrupt.ContextWithIsExternalConnection(ctx)
	if outboundHandler, isHandler := selected.(adapter.PacketConnectionHandlerEx); isHandler {
		outboundHandler.NewPacketConnectionEx(ctx, conn, metadata, onClose)
	} else {
		s.connection.NewPacketConnection(ctx, selected, conn, metadata, onClose)
	}
}

// Close implements adapter.Service
func (s *LoadBalance) Close() error {
	if s.Balancer == nil {
		return nil
	}
	return s.Balancer.Close()
}

// Start implements adapter.Service
func (s *LoadBalance) Start() error {
	if err := s.InitProviders(s.outbound, s.provider); err != nil {
		return err
	}
	b, err := balancer.New(s.ctx, s.router, s.outbound, s.Providers(), &s.options, s.logger)
	if err != nil {
		return err
	}
	s.Balancer = b
	return s.Balancer.Start()
}
