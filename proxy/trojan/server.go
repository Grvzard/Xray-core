package trojan

import (
	"context"
	"io"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	udp_proto "github.com/xtls/xray-core/common/protocol/udp"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/udp"
)

func init() {
	common.Must(common.RegisterConfig((*ServerConfig)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return NewServer(ctx, config.(*ServerConfig))
	}))
}

// Server is an inbound connection handler that handles messages in trojan protocol.
type Server struct {
	policyManager policy.Manager
	validator     *Validator
	fallbacks     proxy.FallbackMap // or nil
	cone          bool
}

// NewServer creates a new trojan inbound handler.
func NewServer(ctx context.Context, config *ServerConfig) (*Server, error) {
	validator := new(Validator)
	for _, user := range config.Users {
		u, err := user.ToMemoryUser()
		if err != nil {
			return nil, newError("failed to get trojan user").Base(err).AtError()
		}

		if err := validator.Add(u); err != nil {
			return nil, newError("failed to add user").Base(err).AtError()
		}
	}

	v := core.MustFromContext(ctx)
	server := &Server{
		policyManager: v.GetFeature(policy.ManagerType()).(policy.Manager),
		validator:     validator,
		cone:          ctx.Value("cone").(bool),
	}

	if config.Fallbacks != nil {
		server.fallbacks = proxy.BuildFallbackMap(config.Fallbacks)
	}

	return server, nil
}

// AddUser implements proxy.UserManager.AddUser().
func (s *Server) AddUser(ctx context.Context, u *protocol.MemoryUser) error {
	return s.validator.Add(u)
}

// RemoveUser implements proxy.UserManager.RemoveUser().
func (s *Server) RemoveUser(ctx context.Context, e string) error {
	return s.validator.Del(e)
}

// Network implements proxy.Inbound.Network().
func (s *Server) Network() []net.Network {
	return []net.Network{net.Network_TCP, net.Network_UNIX}
}

// Process implements proxy.Inbound.Process().
func (s *Server) Process(ctx context.Context, network net.Network, conn stat.Connection, dispatcher routing.Dispatcher) error {
	sid := session.ExportIDToError(ctx)

	iConn := conn
	statConn, ok := iConn.(*stat.CounterConnection)
	if ok {
		iConn = statConn.Connection
	}

	sessionPolicy := s.policyManager.ForLevel(0)
	if err := conn.SetReadDeadline(time.Now().Add(sessionPolicy.Timeouts.Handshake)); err != nil {
		return newError("unable to set read deadline").Base(err).AtWarning()
	}

	first := buf.FromBytes(make([]byte, buf.Size))
	first.Clear()
	var firstLen int64
	if len, err := first.ReadFrom(conn); err != nil {
		return newError("failed to read first request").Base(err)
	} else {
		firstLen = len
	}
	newError("firstLen = ", firstLen).AtInfo().WriteToLog(sid)

	bufferedReader := &buf.BufferedReader{
		Reader: buf.NewReader(conn),
		Buffer: buf.MultiBuffer{first},
	}

	var user *protocol.MemoryUser

	var err *errors.Error
	if firstLen < 58 || first.Byte(56) != '\r' {
		// invalid protocol
		err = newError("not trojan protocol")
		log.Record(&log.AccessMessage{
			From:   conn.RemoteAddr(),
			To:     "",
			Status: log.AccessRejected,
			Reason: err,
		})
	} else {
		user = s.validator.Get(hexString(first.BytesTo(56)))
		if user == nil {
			// invalid user, let's fallback
			err = newError("not a valid user")
			log.Record(&log.AccessMessage{
				From:   conn.RemoteAddr(),
				To:     "",
				Status: log.AccessRejected,
				Reason: err,
			})
		}
	}

	if err != nil {
		if s.fallbacks == nil {
			return err
		}
		newError("fallback starts").Base(err).AtInfo().WriteToLog(sid)
		return proxy.ApplyFallback(ctx, sessionPolicy, conn, iConn, s.fallbacks, first, firstLen, bufferedReader)
	}

	clientReader := &ConnReader{Reader: bufferedReader}
	if err := clientReader.ParseHeader(); err != nil {
		log.Record(&log.AccessMessage{
			From:   conn.RemoteAddr(),
			To:     "",
			Status: log.AccessRejected,
			Reason: err,
		})
		return newError("failed to create request from: ", conn.RemoteAddr()).Base(err)
	}

	destination := clientReader.Target
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return newError("unable to set read deadline").Base(err).AtWarning()
	}

	inbound := session.InboundFromContext(ctx)
	inbound.Name = "trojan"
	inbound.CanSpliceCopy = 3
	inbound.User = user
	sessionPolicy = s.policyManager.ForLevel(user.Level)

	if destination.Network == net.Network_UDP { // handle udp request
		return s.handleUDPPayload(ctx, &PacketReader{Reader: clientReader}, &PacketWriter{Writer: conn}, dispatcher)
	}

	ctx = log.ContextWithAccessMessage(ctx, &log.AccessMessage{
		From:   conn.RemoteAddr(),
		To:     destination,
		Status: log.AccessAccepted,
		Reason: "",
		Email:  user.Email,
	})

	newError("received request for ", destination).WriteToLog(sid)
	return s.handleConnection(ctx, sessionPolicy, destination, clientReader, buf.NewWriter(conn), dispatcher)
}

func (s *Server) handleUDPPayload(ctx context.Context, clientReader *PacketReader, clientWriter *PacketWriter, dispatcher routing.Dispatcher) error {
	udpServer := udp.NewDispatcher(dispatcher, func(ctx context.Context, packet *udp_proto.Packet) {
		udpPayload := packet.Payload
		if udpPayload.UDP == nil {
			udpPayload.UDP = &packet.Source
		}

		if err := clientWriter.WriteMultiBuffer(buf.MultiBuffer{udpPayload}); err != nil {
			newError("failed to write response").Base(err).AtWarning().WriteToLog(session.ExportIDToError(ctx))
		}
	})

	inbound := session.InboundFromContext(ctx)
	user := inbound.User

	var dest *net.Destination

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			mb, err := clientReader.ReadMultiBuffer()
			if err != nil {
				if errors.Cause(err) != io.EOF {
					return newError("unexpected EOF").Base(err)
				}
				return nil
			}

			mb2, b := buf.SplitFirst(mb)
			if b == nil {
				continue
			}
			destination := *b.UDP

			currentPacketCtx := ctx
			if inbound.Source.IsValid() {
				currentPacketCtx = log.ContextWithAccessMessage(ctx, &log.AccessMessage{
					From:   inbound.Source,
					To:     destination,
					Status: log.AccessAccepted,
					Reason: "",
					Email:  user.Email,
				})
			}
			newError("tunnelling request to ", destination).WriteToLog(session.ExportIDToError(ctx))

			if !s.cone || dest == nil {
				dest = &destination
			}

			udpServer.Dispatch(currentPacketCtx, *dest, b) // first packet
			for _, payload := range mb2 {
				udpServer.Dispatch(currentPacketCtx, *dest, payload)
			}
		}
	}
}

func (s *Server) handleConnection(ctx context.Context, sessionPolicy policy.Session,
	destination net.Destination,
	clientReader buf.Reader,
	clientWriter buf.Writer, dispatcher routing.Dispatcher,
) error {
	ctx, cancel := context.WithCancel(ctx)
	timer := signal.CancelAfterInactivity(ctx, cancel, sessionPolicy.Timeouts.ConnectionIdle)
	ctx = policy.ContextWithBufferPolicy(ctx, sessionPolicy.Buffer)

	link, err := dispatcher.Dispatch(ctx, destination)
	if err != nil {
		return newError("failed to dispatch request to ", destination).Base(err)
	}

	requestDone := func() error {
		defer timer.SetTimeout(sessionPolicy.Timeouts.DownlinkOnly)
		if buf.Copy(clientReader, link.Writer, buf.UpdateActivity(timer)) != nil {
			return newError("failed to transfer request").Base(err)
		}
		return nil
	}

	responseDone := func() error {
		defer timer.SetTimeout(sessionPolicy.Timeouts.UplinkOnly)

		if err := buf.Copy(link.Reader, clientWriter, buf.UpdateActivity(timer)); err != nil {
			return newError("failed to write response").Base(err)
		}
		return nil
	}

	requestDonePost := task.OnSuccess(requestDone, task.Close(link.Writer))
	if err := task.Run(ctx, requestDonePost, responseDone); err != nil {
		common.Must(common.Interrupt(link.Reader))
		common.Must(common.Interrupt(link.Writer))
		return newError("connection ends").Base(err)
	}

	return nil
}
