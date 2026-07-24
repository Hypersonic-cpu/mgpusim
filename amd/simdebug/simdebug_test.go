package simdebug

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func resetLogger(t *testing.T) {
	t.Helper()
	if err := Close(); err != nil {
		t.Fatalf("close logger: %v", err)
	}
	t.Cleanup(func() {
		if err := Close(); err != nil {
			t.Fatalf("close logger: %v", err)
		}
	})
}

func TestDisabledByDefault(t *testing.T) {
	resetLogger(t)

	var output bytes.Buffer
	if err := Configure(Config{Writer: &output}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	DPrintf(VMMap, "pid=%d", 1)

	if output.Len() != 0 {
		t.Fatalf("disabled logger wrote %q", output.String())
	}
}

func TestMultipleCategories(t *testing.T) {
	resetLogger(t)

	var output bytes.Buffer
	if err := Configure(Config{Flags: []Flag{VMMap, TLBFill}, Writer: &output}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	DPrintf(VMMap, "map")
	DPrintf(TLBFill, "fill")
	DPrintf(Fault, "fault")

	got := output.String()
	if got != "[VMMap] map\n[TLBFill] fill\n" {
		t.Fatalf("unexpected output %q", got)
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	resetLogger(t)

	err := Configure(Config{Flags: []Flag{"unknown"}})
	if !errors.Is(err, ErrUnknownFlag) {
		t.Fatalf("expected unknown flag error, got %v", err)
	}
}

func TestConfigureFromEnv(t *testing.T) {
	resetLogger(t)
	t.Setenv("MGPUSIM_DEBUG", "VMMap,TLBFill")
	t.Setenv("MGPUSIM_DEBUG_MAX_BYTES", "128")

	if err := ConfigureFromEnv(); err != nil {
		t.Fatalf("configure from environment: %v", err)
	}
	if !Enabled(VMMap) || !Enabled(TLBFill) || Enabled(Fault) {
		t.Fatal("environment categories were not applied")
	}
}

func TestOutputRedirection(t *testing.T) {
	resetLogger(t)

	path := filepath.Join(t.TempDir(), "debug.log")
	if err := Configure(Config{Flags: []Flag{VMMap}, File: path}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	DPrintf(VMMap, "pid=%d", 7)
	if err := Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "[VMMap] pid=7\n" {
		t.Fatalf("unexpected file content %q", data)
	}
}

func TestConcurrentWrites(t *testing.T) {
	resetLogger(t)

	var output bytes.Buffer
	if err := Configure(Config{Flags: []Flag{VMMap}, Writer: &output}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	const writers = 64
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			DPrintf(VMMap, "mapping")
		}()
	}
	wg.Wait()

	if got := strings.Count(output.String(), "[VMMap] mapping\n"); got != writers {
		t.Fatalf("expected %d complete lines, got %d", writers, got)
	}
}

func TestMaximumByteBehavior(t *testing.T) {
	resetLogger(t)

	var output bytes.Buffer
	if err := Configure(Config{Flags: []Flag{VMMap}, Writer: &output, MaxBytes: 14}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	DPrintf(VMMap, "one")
	DPrintf(VMMap, "two")

	if got := output.String(); got != "[VMMap] one\n" {
		t.Fatalf("unexpected output %q", got)
	}
	if !errors.Is(Error(), ErrMaxBytesExceeded) {
		t.Fatalf("expected max-byte error, got %v", Error())
	}
	if err := Close(); !errors.Is(err, ErrMaxBytesExceeded) {
		t.Fatalf("expected max-byte close error, got %v", err)
	}
}
