package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// State represents the persisted download state.
// Maps URL -> bytes downloaded so far (for resume support).
type State map[string]int64

const (
	stateFileName   = "ak.state.json"
	stateSaveInterval = 2 * time.Second // Save state every 2 seconds
	stateSaveBytes    = 1024 * 1024      // Save state every 1 MB written
)

// stateManager handles reading/writing the state file with mutex protection.
type stateManager struct {
	mu    sync.Mutex
	state State
	path  string
}

// loadState reads the state file from disk, returning an empty state on error.
func (sm *stateManager) loadState() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	data, err := os.ReadFile(sm.path)
	if err != nil {
		if os.IsNotExist(err) {
			sm.state = make(State)
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	if len(data) == 0 {
		sm.state = make(State)
		return nil
	}

	if err := json.Unmarshal(data, &sm.state); err != nil {
		return fmt.Errorf("failed to parse state file: %w", err)
	}
	return nil
}

// saveState writes the current state to disk.
func (sm *stateManager) saveState() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	data, err := json.MarshalIndent(sm.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(sm.path, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	return nil
}

// getResumedBytes returns the number of bytes already downloaded for a URL.
// Returns 0 if no partial download exists.
func (sm *stateManager) getResumedBytes(url string) int64 {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state[url]
}

// setResumedBytes updates the bytes downloaded for a URL in memory.
func (sm *stateManager) setResumedBytes(url string, bytes int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.state[url] = bytes
}

// getBytes returns the current byte count without locking (for periodic saves).
func (sm *stateManager) getBytes(url string) int64 {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state[url]
}

// removeEntry deletes the URL entry from state (called on successful completion).
func (sm *stateManager) removeEntry(url string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.state, url)
}

// progressWriter wraps an io.Writer and displays download progress.
// It shows bytes downloaded and total bytes (if Content-Length is available).
// It also triggers periodic state saves for resume support.
type progressWriter struct {
	writer       io.Writer
	url          string
	downloaded   int64
	total        int64
	lastPrinted  time.Time
	lastStateSave int64 // Track bytes at last state save
	stateManager *stateManager
	lastStateSync time.Time
	mu           sync.Mutex
	done         chan struct{} // Closed when download completes
}

// newProgressWriter creates a new progress tracker.
func newProgressWriter(w io.Writer, total int64, sm *stateManager, url string) *progressWriter {
	return &progressWriter{
		writer:       w,
		downloaded:   0,
		total:        total,
		lastPrinted:  time.Now(),
		lastStateSave: 0,
		stateManager: sm,
		lastStateSync: time.Now(),
		done:         make(chan struct{}),
	}
}

// Write implements io.Writer, updating progress and saving state periodically.
func (pw *progressWriter) Write(p []byte) (n int, err error) {
	pw.mu.Lock()
	pw.downloaded += int64(len(p))
	pw.mu.Unlock()

	now := time.Now()

	// Throttle console output to avoid flickering (update every 100ms).
	if now.Sub(pw.lastPrinted) >= 100*time.Millisecond {
		pw.mu.Lock()
		pw.printProgress()
		pw.lastPrinted = now
		pw.mu.Unlock()
	}

	// Periodically save state: every 2 seconds OR every 1 MB written.
	shouldSave := false
	pw.mu.Lock()
	if pw.downloaded-pw.lastStateSave >= stateSaveBytes {
		shouldSave = true
		pw.lastStateSave = pw.downloaded
	} else if now.Sub(pw.lastStateSync) >= stateSaveInterval {
		shouldSave = true
	}
	if shouldSave {
		pw.lastStateSync = now
		sm := pw.stateManager
		sm.mu.Lock()
		sm.state[pw.url] = pw.downloaded
		sm.mu.Unlock()
		// Save asynchronously to not block writes.
		go func() {
			if err := pw.stateManager.saveState(); err != nil {
				fmt.Fprintf(os.Stderr, "\nWarning: failed to save state: %v\n", err)
			}
		}()
	}
	pw.mu.Unlock()

	// Write to actual destination (os.Stdout or file).
	return pw.writer.Write(p)
}

// printProgress displays the current download progress to stderr.
func (pw *progressWriter) printProgress() {
	prefix := fmt.Sprintf("  %s", pw.url)
	if pw.total > 0 {
		percentage := float64(pw.downloaded) / float64(pw.total) * 100
		fmt.Fprintf(os.Stderr, "\r%s: %s / %s (%.1f%%)\033[K", prefix,
			formatBytes(pw.downloaded), formatBytes(pw.total), percentage)
	} else {
		fmt.Fprintf(os.Stderr, "\r%s: %s\033[K", prefix, formatBytes(pw.downloaded))
	}
}

// finalize prints a newline after download completes.
func (pw *progressWriter) finalize() {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	fmt.Fprintln(os.Stderr) // Move to next line
	close(pw.done)         // Signal periodic saver to stop
}

// getDownloaded returns the current byte count.
func (pw *progressWriter) getDownloaded() int64 {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return pw.downloaded
}

// formatBytes converts bytes to a human-readable string (KB, MB, GB, etc.).
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// isNetworkError checks if an error is a network/connection error.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	networkErrors := []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"eof",
		"timeout",
		"use of closed network connection",
		"client connection",
		"server closed",
	}
	for _, ne := range networkErrors {
		if strings.Contains(strings.ToLower(errStr), ne) {
			return true
		}
	}
	return false
}

// parseURL is a simple URL parser using net/url.
func parseURL(rawURL string) (*url.URL, error) {
	return url.Parse(rawURL)
}

// fetchCommand downloads a file from the given URL with resume support.
func fetchCommand(url string, sm *stateManager) error {
	// Validate URL.
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("invalid URL: must start with http:// or https://")
	}

	// Determine output filename from URL path.
	parsedURL, err := parseURL(url)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	filename := filepath.Base(parsedURL.Path)
	if filename == "" || filename == "." {
		filename = "download"
	}

	// Check for partial download to resume.
	partialBytes := sm.getResumedBytes(url)

	// Create HTTP client with timeout.
	client := &http.Client{
		Timeout: 5 * time.Minute, // Allow long downloads.
	}

	// Prepare request with Range header if resuming.
	var req *http.Request
	req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if partialBytes > 0 {
		// Request resume from byte partialBytes onward.
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", partialBytes))
		fmt.Fprintf(os.Stderr, "Resuming download from byte %d...\n", partialBytes)
	}

	// Execute request.
	resp, err := client.Do(req)
	if err != nil {
		// Network error: leave state intact for resume on next run.
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	// Check HTTP status.
	totalSize := resp.ContentLength
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 - Server supports resume.
		fmt.Fprintf(os.Stderr, "Server supports resume (206 Partial Content)\n")
		// ContentLength in 206 is the remaining bytes, not total.
		// Adjust total for progress display.
		if partialBytes > 0 && totalSize > 0 {
			totalSize = partialBytes + totalSize
		}
	case http.StatusOK: // 200 - Server ignored Range header, full download.
		if partialBytes > 0 {
			fmt.Fprintf(os.Stderr, "Server does not support resume (200 OK), restarting from beginning\n")
			partialBytes = 0
			// Discard any partial file.
			if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove partial file: %w", err)
			}
		}
	default:
		return fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	// Open output file (append mode if resuming).
	var file *os.File
	openFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if partialBytes > 0 {
		// Append to existing partial file.
		openFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err = os.OpenFile(filename, openFlags, 0644)
	if err != nil {
		return fmt.Errorf("failed to open output file: %w", err)
	}

	// Set up signal handling for graceful interrupt.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Create progress writer with state manager reference.
	pw := newProgressWriter(file, totalSize, sm, url)

	// Copy with progress tracking, wrapped to handle signals and panics.
	var bytesWritten int64
	copyDone := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				copyDone <- fmt.Errorf("recovered from panic during copy: %v", r)
			}
		}()
		n, err := io.Copy(pw, resp.Body)
		bytesWritten = n
		copyDone <- err
	}()

	// Wait for either copy completion or signal.
	var downloadErr error
	select {
	case downloadErr = <-copyDone:
		// Copy completed (success or error).
	case sig := <-sigChan:
		// Signal received - save state and exit gracefully.
		fmt.Fprintf(os.Stderr, "\n\nReceived %v, saving progress...\n", sig)

		// Stop the copy by closing the response body.
		resp.Body.Close()

		// Get current progress and save state.
		currentBytes := pw.getDownloaded()
		sm.setResumedBytes(url, partialBytes+currentBytes)
		if err := sm.saveState(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save state: %v\n", err)
		}

		file.Close()
		pw.finalize()
		fmt.Fprintf(os.Stderr, "Saved %s of progress to %s (run again to resume)\n",
			formatBytes(currentBytes), stateFileName)
		os.Exit(130) // Standard exit code for SIGINT.
		return nil
	}

	// Close file and finalize progress display.
	file.Close()
	pw.finalize()

	if downloadErr != nil {
		// Network dropped mid-download: save state for resume.
		if downloadErr != io.EOF && !isNetworkError(downloadErr) {
			// Actual error (not just connection drop).
			sm.setResumedBytes(url, partialBytes+bytesWritten)
			sm.saveState()
			return fmt.Errorf("download failed after %d bytes: %w", partialBytes+bytesWritten, downloadErr)
		}
		// Connection dropped: state preserved for resume.
		sm.setResumedBytes(url, partialBytes+bytesWritten)
		sm.saveState()
		return fmt.Errorf("connection lost after %d bytes (run again to resume)", partialBytes+bytesWritten)
	}

	// Download completed successfully.
	fmt.Fprintf(os.Stderr, "Downloaded %s to %s\n", formatBytes(bytesWritten), filename)

	// Remove entry from state file.
	sm.removeEntry(url)
	if err := sm.saveState(); err != nil {
		// Non-fatal: download succeeded, just couldn't clean up state.
		fmt.Fprintf(os.Stderr, "Warning: failed to clean up state file: %v\n", err)
	}

	return nil
}

// usage prints command usage.
func usage() {
	fmt.Fprintf(os.Stderr, `ak - A simple file downloader with resume support

Usage:
  ak fetch <url>    Download a file from <url>

Example:
  ak fetch https://example.com/file.zip

Notes:
  - Downloads are automatically resumed if interrupted.
  - State is stored in %s
  - Progress is saved periodically and on Ctrl+C.
  - Progress indicator is shown in the terminal.

`, stateFileName)
}

func main() {
	// Determine state file path (in current working directory).
	statePath := stateFileName

	// Initialize state manager.
	sm := &stateManager{
		path:  statePath,
		state: make(State),
	}
	if err := sm.loadState(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v (starting fresh)\n", err)
	}

	// Parse command line arguments.
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "fetch":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Error: fetch requires a URL argument\n\n")
			usage()
			os.Exit(1)
		}
		url := os.Args[2]
		if err := fetchCommand(url, sm); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}
