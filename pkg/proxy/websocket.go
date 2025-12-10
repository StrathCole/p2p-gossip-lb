package proxy

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nhooyr.io/websocket"

	"github.com/lunc/mesh/pkg/selector"
)

func (p *Proxy) proxyWebsocket(w http.ResponseWriter, r *http.Request, candidate selector.Candidate) error {
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	defer clientConn.Close(websocket.StatusInternalError, "proxy error")

	scheme := "wss"
	if strings.EqualFold(p.backendScheme, "http") {
		scheme = "ws"
	}

	backendURL := url.URL{
		Scheme:   scheme,
		Host:     candidate.Meta.Host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}

	header := http.Header{}
	if sub := clientConn.Subprotocol(); sub != "" {
		header.Set("Sec-WebSocket-Protocol", sub)
	}
	if forwarded := appendForwardedFor(r.Header.Get("X-Forwarded-For"), clientIPFromRequest(r)); forwarded != "" {
		header.Set("X-Forwarded-For", forwarded)
	}
	header.Set("X-Mesh-Backend", string(candidate.Meta.ID))

	dialCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	backendConn, _, err := websocket.Dial(dialCtx, backendURL.String(), &websocket.DialOptions{HTTPHeader: header})
	cancel()
	if err != nil {
		clientConn.CloseNow()
		return err
	}
	defer backendConn.Close(websocket.StatusInternalError, "proxy error")

	ctx, cancelCopy := context.WithCancel(r.Context())
	defer cancelCopy()

	errCh := make(chan error, 2)
	go relayWebsocket(ctx, clientConn, backendConn, errCh)
	go relayWebsocket(ctx, backendConn, clientConn, errCh)

	err = <-errCh
	cancelCopy()
	if err != nil && !isNormalClosure(err) {
		backendConn.Close(websocket.StatusInternalError, "proxy error")
		clientConn.Close(websocket.StatusInternalError, "proxy error")
		return err
	}

	backendConn.Close(websocket.StatusNormalClosure, "")
	clientConn.Close(websocket.StatusNormalClosure, "")

	select {
	case <-errCh:
	default:
	}

	return nil
}

func relayWebsocket(ctx context.Context, src, dst *websocket.Conn, errCh chan<- error) {
	for {
		msgType, data, err := src.Read(ctx)
		if err != nil {
			errCh <- err
			return
		}
		if err := dst.Write(ctx, msgType, data); err != nil {
			errCh <- err
			return
		}
	}
}

func isNormalClosure(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	default:
		return false
	}
}
