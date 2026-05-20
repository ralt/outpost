package ctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/ralt/outpost/internal/logging"
)

// Handler dispatches RPCs by method name. Each handler returns either a data
// payload (marshalled to JSON) or a structured error.
type Handler interface {
	HandleSendAway(ctx context.Context, args SendAwayArgs) (SendAwayResult, error)
	HandleBringBack(ctx context.Context, args BringBackArgs) (BringBackResult, error)
	HandleStatus(ctx context.Context) (StatusResult, error)
	HandleProjects(ctx context.Context) (ProjectsResult, error)
	HandleReload(ctx context.Context) error
}

// CodedError lets a handler return a stable error code with a human message.
type CodedError struct {
	Code string
	Msg  string
}

func (e *CodedError) Error() string { return e.Msg }

func NewError(code, msg string) error { return &CodedError{Code: code, Msg: msg} }

func NewErrorf(code, format string, args ...any) error {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Server is the unix-socket RPC accept loop.
type Server struct {
	socket   string
	log      *slog.Logger
	handler  Handler
	listener net.Listener
	closed   atomic.Bool
}

func NewServer(socket string, h Handler, log *slog.Logger) *Server {
	return &Server{socket: socket, handler: h, log: logging.WithComponent(log, logging.CompCtl)}
}

// Listen unlinks any stale socket and binds anew. It does *not* start serving.
func (s *Server) Listen() error {
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o700); err != nil {
		return fmt.Errorf("ctl: mkdir socket dir: %w", err)
	}
	if err := unlinkStale(s.socket); err != nil {
		return err
	}
	l, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("ctl: listen: %w", err)
	}
	if err := os.Chmod(s.socket, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("ctl: chmod socket: %w", err)
	}
	s.listener = l
	s.log.Info("control-socket listening", "path", s.socket)
	return nil
}

// Serve accepts and dispatches until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("ctl: Listen() not called")
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("accept error", "err", err)
			continue
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Server) Close() error {
	s.closed.Store(true)
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.socket)
	return nil
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		s.writeErr(conn, "", CodeBadArgs, fmt.Sprintf("read: %v", err))
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeErr(conn, "", CodeBadArgs, fmt.Sprintf("parse: %v", err))
		return
	}
	traceID := logging.NewTraceID()
	log := s.log.With("req", traceID)
	log.Info("rpc", "method", req.Method)

	resp := s.dispatch(ctx, log, traceID, req)
	resp.Req = traceID
	enc := json.NewEncoder(conn)
	if err := enc.Encode(&resp); err != nil {
		log.Warn("write response", "err", err)
	}
}

func (s *Server) dispatch(ctx context.Context, log *slog.Logger, traceID string, req Request) Response {
	ctx = context.WithValue(ctx, traceIDKey{}, traceID)
	switch req.Method {
	case "send-away":
		var args SendAwayArgs
		if len(req.Args) > 0 {
			if err := json.Unmarshal(req.Args, &args); err != nil {
				return errResp(CodeBadArgs, err.Error())
			}
		}
		data, err := s.handler.HandleSendAway(ctx, args)
		if err != nil {
			return errFrom(err)
		}
		return okResp(data)
	case "bring-back":
		var args BringBackArgs
		if len(req.Args) > 0 {
			if err := json.Unmarshal(req.Args, &args); err != nil {
				return errResp(CodeBadArgs, err.Error())
			}
		}
		data, err := s.handler.HandleBringBack(ctx, args)
		if err != nil {
			return errFrom(err)
		}
		return okResp(data)
	case "status":
		data, err := s.handler.HandleStatus(ctx)
		if err != nil {
			return errFrom(err)
		}
		return okResp(data)
	case "projects":
		data, err := s.handler.HandleProjects(ctx)
		if err != nil {
			return errFrom(err)
		}
		return okResp(data)
	case "reload":
		if err := s.handler.HandleReload(ctx); err != nil {
			return errFrom(err)
		}
		return Response{OK: true}
	default:
		return errResp(CodeUnknownMethod, fmt.Sprintf("unknown method: %q", req.Method))
	}
}

type traceIDKey struct{}

// TraceID returns the request id stashed by the dispatch path.
func TraceID(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey{}).(string); ok {
		return v
	}
	return ""
}

func okResp(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		return errResp(CodeInternal, err.Error())
	}
	return Response{OK: true, Data: b}
}

func errResp(code, msg string) Response {
	return Response{OK: false, Code: code, Error: msg}
}

func errFrom(err error) Response {
	var ce *CodedError
	if errors.As(err, &ce) {
		return errResp(ce.Code, ce.Msg)
	}
	return errResp(CodeInternal, err.Error())
}

func (s *Server) writeErr(conn net.Conn, traceID, code, msg string) {
	r := errResp(code, msg)
	r.Req = traceID
	_ = json.NewEncoder(conn).Encode(&r)
}

func unlinkStale(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("ctl: stat existing socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("ctl: %s exists and is not a socket; refusing to clobber", path)
	}
	c, err := net.DialTimeout("unix", path, dialProbeTimeout)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("ctl: another outpost daemon is already listening on %s", path)
	}
	return os.Remove(path)
}
