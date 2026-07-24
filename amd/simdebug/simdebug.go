// Package simdebug provides opt-in, category-based simulator debug logging.
package simdebug

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Flag identifies a debug-log category.
type Flag string

const (
	// VMMap reports virtual-to-physical mapping establishment.
	VMMap Flag = "VMMap"
	// PageTable reports page-table construction and updates.
	PageTable Flag = "PageTable"
	// GMMUWalk reports page-table-walker progress.
	GMMUWalk Flag = "GMMUWalk"
	// PWC reports page-walk-cache activity.
	PWC Flag = "PWC"
	// PTWMem reports page-table-walk memory requests.
	PTWMem Flag = "PTWMem"
	// TLBReplay reports requests replayed after translation.
	TLBReplay Flag = "TLBReplay"
	// TLBFill reports TLB fills and their waiters.
	TLBFill Flag = "TLBFill"
	// Fault reports translation faults.
	Fault Flag = "Fault"
)

var (
	// ErrUnknownFlag is returned when configuration names an unsupported flag.
	ErrUnknownFlag = errors.New("unknown simulation debug flag")
	// ErrMaxBytesExceeded is reported once a log line would exceed MaxBytes.
	ErrMaxBytesExceeded = errors.New("simulation debug output byte limit exceeded")
)

var knownFlags = map[Flag]struct{}{
	VMMap: {}, PageTable: {}, GMMUWalk: {}, PWC: {}, PTWMem: {},
	TLBReplay: {}, TLBFill: {}, Fault: {},
}

// Config controls debug-log categories and destination. Exactly one of Writer
// and File may be set. MaxBytes of zero means unlimited output.
type Config struct {
	Flags    []Flag
	Writer   io.Writer
	File     string
	MaxBytes int64
}

type logger struct {
	mu sync.RWMutex

	flags     map[Flag]struct{}
	writer    io.Writer
	ownedFile *os.File
	maxBytes  int64
	bytes     int64
	err       error
}

var global = logger{
	flags:  make(map[Flag]struct{}),
	writer: io.Discard,
}

// Configure replaces the current debug configuration.
func Configure(cfg Config) error {
	if cfg.MaxBytes < 0 {
		return fmt.Errorf("simdebug: MaxBytes must not be negative")
	}
	if cfg.Writer != nil && cfg.File != "" {
		return fmt.Errorf("simdebug: Writer and File cannot both be set")
	}

	flags := make(map[Flag]struct{}, len(cfg.Flags))
	for _, flag := range cfg.Flags {
		if _, ok := knownFlags[flag]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownFlag, flag)
		}
		flags[flag] = struct{}{}
	}

	writer := cfg.Writer
	var ownedFile *os.File
	if cfg.File != "" {
		file, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("simdebug: open debug file: %w", err)
		}
		writer = file
		ownedFile = file
	}
	if writer == nil {
		if len(flags) == 0 {
			writer = io.Discard
		} else {
			writer = os.Stderr
		}
	}

	global.mu.Lock()
	previousFile := global.ownedFile
	global.flags = flags
	global.writer = writer
	global.ownedFile = ownedFile
	global.maxBytes = cfg.MaxBytes
	global.bytes = 0
	global.err = nil
	global.mu.Unlock()

	if previousFile != nil {
		if err := previousFile.Close(); err != nil {
			return fmt.Errorf("simdebug: close previous debug file: %w", err)
		}
	}

	return nil
}

// ConfigureFromEnv applies MGPUSIM_DEBUG, MGPUSIM_DEBUG_FILE, and
// MGPUSIM_DEBUG_MAX_BYTES. It does nothing when none of the variables is set.
func ConfigureFromEnv() error {
	flagsValue, flagsSet := os.LookupEnv("MGPUSIM_DEBUG")
	file, fileSet := os.LookupEnv("MGPUSIM_DEBUG_FILE")
	maxBytesValue, maxBytesSet := os.LookupEnv("MGPUSIM_DEBUG_MAX_BYTES")
	if !flagsSet && !fileSet && !maxBytesSet {
		return nil
	}

	cfg := Config{File: file}
	if strings.TrimSpace(flagsValue) != "" {
		for _, value := range strings.Split(flagsValue, ",") {
			cfg.Flags = append(cfg.Flags, Flag(strings.TrimSpace(value)))
		}
	}
	if maxBytesSet && maxBytesValue != "" {
		maxBytes, err := strconv.ParseInt(maxBytesValue, 10, 64)
		if err != nil {
			return fmt.Errorf("simdebug: parse MGPUSIM_DEBUG_MAX_BYTES: %w", err)
		}
		cfg.MaxBytes = maxBytes
	}

	return Configure(cfg)
}

// Enabled reports whether a debug category is enabled.
func Enabled(flag Flag) bool {
	global.mu.RLock()
	_, ok := global.flags[flag]
	global.mu.RUnlock()
	return ok
}

// DPrintf writes a category-prefixed log line when flag is enabled. It avoids
// formatting unless the category is enabled. Callers with expensive arguments
// should still guard their construction with Enabled.
func DPrintf(flag Flag, format string, args ...any) {
	if !Enabled(flag) {
		return
	}

	line := fmt.Sprintf("[%s] %s\n", flag, fmt.Sprintf(format, args...))

	global.mu.Lock()
	defer global.mu.Unlock()
	if _, ok := global.flags[flag]; !ok || global.err != nil {
		return
	}
	if global.maxBytes > 0 && global.bytes+int64(len(line)) > global.maxBytes {
		global.err = fmt.Errorf("%w: limit=%d written=%d attempted=%d",
			ErrMaxBytesExceeded, global.maxBytes, global.bytes, len(line))
		return
	}

	n, err := io.WriteString(global.writer, line)
	global.bytes += int64(n)
	if err != nil {
		global.err = fmt.Errorf("simdebug: write debug output: %w", err)
	}
}

// BytesWritten returns the number of successfully written debug bytes.
func BytesWritten() int64 {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.bytes
}

// Error returns the first write or byte-limit error, if any.
func Error() error {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.err
}

// Close flushes and closes an owned output file, disables all flags, and
// returns any stored write or byte-limit error.
func Close() error {
	global.mu.Lock()
	file := global.ownedFile
	err := global.err
	global.flags = make(map[Flag]struct{})
	global.writer = io.Discard
	global.ownedFile = nil
	global.maxBytes = 0
	global.bytes = 0
	global.err = nil
	global.mu.Unlock()

	if file != nil {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("simdebug: close debug file: %w", closeErr)
		}
	}

	return err
}
