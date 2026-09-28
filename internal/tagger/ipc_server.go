//go:build tagger

package tagger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/monbooru/monbooru/internal/logx"
)

func RunWorkerServer(ctx context.Context, addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("tagger-worker: dial %q: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := writeFrame(conn, ipcHello{Token: os.Getenv(ipcTokenEnv)}); err != nil {
		return fmt.Errorf("tagger-worker: handshake: %w", err)
	}
	logx.Infof("tagger-worker: connected to parent addr=%s", addr)
	return serveRequests(ctx, conn)
}

func serveRequests(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Every write goes through writeMu: Run's progress callbacks write
	// from worker goroutines.
	var writeMu sync.Mutex
	for {
		var req ipcRequest
		if err := readFrame(conn, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("tagger-worker: read: %w", err)
		}
		if req.Method == ipcMethodShutdown {
			if err := writeLocked(conn, &writeMu, ipcResponse{}); err != nil {
				return fmt.Errorf("tagger-worker: shutdown response: %w", err)
			}
			cancel()
			return nil
		}
		if err := handle(ctx, req, conn, &writeMu); err != nil {
			return err
		}
	}
}

func handle(ctx context.Context, req ipcRequest, conn net.Conn, writeMu *sync.Mutex) error {
	switch req.Method {
	case ipcMethodRun:
		return handleRun(ctx, req, conn, writeMu)
	case ipcMethodStatus:
		st := defaultBackend.Status()
		return writeLocked(conn, writeMu, ipcResponse{Status: &st})
	case ipcMethodReleaseIdle:
		released := defaultBackend.ReleaseIdle(req.IdleAfter)
		return writeLocked(conn, writeMu, ipcResponse{Released: released})
	case ipcMethodReleaseAll:
		defaultBackend.ReleaseAll()
		return writeLocked(conn, writeMu, ipcResponse{})
	}
	return writeLocked(conn, writeMu, ipcResponse{Err: fmt.Sprintf("unknown method %d", req.Method)})
}

func handleRun(ctx context.Context, req ipcRequest, conn net.Conn, writeMu *sync.Mutex) error {
	if req.Run == nil {
		return writeLocked(conn, writeMu, ipcResponse{Err: "run: empty request"})
	}
	runReq := *req.Run
	runReq.OnProgress = func(workerIdx int, msg string) {
		_ = writeLocked(conn, writeMu, ipcResponse{Stream: true, WorkerIdx: workerIdx, Msg: msg})
	}
	resp, err := defaultBackend.Run(ctx, runReq)
	if err != nil {
		return writeLocked(conn, writeMu, ipcResponse{Err: err.Error()})
	}
	return writeLocked(conn, writeMu, ipcResponse{Run: &resp})
}

func writeLocked(conn net.Conn, writeMu *sync.Mutex, resp ipcResponse) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	return writeFrame(conn, resp)
}
