package client_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
)

func TestWaitRevCtxReturnsOnCancelRatherThanAtTheDeadline(t *testing.T) {
	const defP = `<defNumberVector device="D" name="P" state="Ok" perm="rw">` +
		`<defNumber name="M" label="M" format="%g" min="0" max="10" step="1">1</defNumber></defNumberVector>`
	c := dialRaw(t, rawServer(t, defP))
	if _, ok := c.Wait("D", "P", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("P never defined")
	}

	// A predicate nothing will satisfy: the server sends no more updates.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.WaitRevCtx(ctx, "D", "P", 1<<62, func(client.Property) bool { return false })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitRevCtx = nil error after cancel, want an error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wait took %v to notice the cancel, want prompt", elapsed)
	}
	// A cancel says nothing about the device; a timeout says it did not answer.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel err = %v, want it to unwrap to context.Canceled", err)
	}
	if strings.Contains(err.Error(), "timeout") {
		t.Errorf("cancel err = %v, want no \"timeout\" (that is the deadline case)", err)
	}
}

func TestWaitRevCtxStillReportsADeadlineAsATimeout(t *testing.T) {
	const defP = `<defNumberVector device="D" name="P" state="Ok" perm="rw">` +
		`<defNumber name="M" label="M" format="%g" min="0" max="10" step="1">1</defNumber></defNumberVector>`
	c := dialRaw(t, rawServer(t, defP))
	if _, ok := c.Wait("D", "P", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("P never defined")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.WaitRevCtx(ctx, "D", "P", 1<<62, func(client.Property) bool { return false })
	if err == nil {
		t.Fatal("WaitRevCtx = nil error after the deadline, want an error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("deadline err = %v, want a timeout message", err)
	}
}

func TestTheTimeoutFormsStillBehaveAsBefore(t *testing.T) {
	const defP = `<defNumberVector device="D" name="P" state="Ok" perm="rw">` +
		`<defNumber name="M" label="M" format="%g" min="0" max="10" step="1">1</defNumber></defNumberVector>`
	c := dialRaw(t, rawServer(t, defP))

	// Wait is satisfied by cached state and returns true.
	if _, ok := c.Wait("D", "P", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("Wait(D.P) = false, want true for a defined property")
	}
	// ...and reports false rather than blocking forever on one that never arrives.
	if _, ok := c.Wait("D", "MISSING", func(client.Property) bool { return true }, 80*time.Millisecond); ok {
		t.Error("Wait(D.MISSING) = true, want false")
	}
	// WaitRev times out with the same message shape callers match on today.
	_, err := c.WaitRev("D", "P", 1<<62, func(client.Property) bool { return false }, 80*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for D.P") {
		t.Errorf("WaitRev err = %v, want %q", err, "timeout waiting for D.P")
	}
	// WaitDevices still finds a device that is present.
	if !c.WaitDevices(1, 3*time.Second) {
		t.Error("WaitDevices(1) = false, want true")
	}
}

func TestWaitCtxReturnsImmediatelyOnCachedState(t *testing.T) {
	const defP = `<defNumberVector device="D" name="P" state="Ok" perm="rw">` +
		`<defNumber name="M" label="M" format="%g" min="0" max="10" step="1">1</defNumber></defNumberVector>`
	c := dialRaw(t, rawServer(t, defP))
	if _, ok := c.Wait("D", "P", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("P never defined")
	}

	// Already cancelled, but the property is cached, so this must still succeed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := c.WaitCtx(ctx, "D", "P", func(client.Property) bool { return true }); !ok {
		t.Error("WaitCtx = false for a cached property on a cancelled context, want true")
	}
}
