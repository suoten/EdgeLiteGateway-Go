package e2e

// A connection the server accepts but that never sends a request header is in
// http.StateNew, and http.Server.Shutdown only force-closes *idle* connections, so
// such a socket pins graceful shutdown until the caller's budget runs out. On the
// gateway that meant the whole 30s grace period burning before the process was
// killed with the databases still open, which one TCP health probe or port scan was
// enough to cause.

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestSilentConnectionCannotPinShutdown(t *testing.T) {
	app := setupSmokeApp(t)
	defer app.teardown(t)

	silent, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", app.port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close()

	// Give the accept goroutine a moment to register the connection as active.
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := app.echo.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown gave up after %v with the connection still silent: %v", time.Since(start), err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("shutdown waited %v for a connection that never sent a byte; the read-header budget is not working", elapsed)
	}
}
