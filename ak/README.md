# ak - A Simple File Downloader with Resume Support

A lightweight Go CLI tool for downloading files with automatic resume capability.

## Building

### Windows

```bash
go build -o ak.exe main.go
```

### Linux/macOS

```bash
go build -o ak main.go
```

### Cross-compilation for Windows from Linux/macOS

```bash
GOOS=windows GOARCH=amd64 go build -o ak.exe main.go
```

## Usage

```
ak fetch <url>
```

### Examples

```bash
# Download a file
ak fetch https://example.com/large-file.zip

# Download from HTTP
ak fetch http://example.com/file.iso
```

## Features

- **Streaming download**: Uses `io.Copy` to stream data directly to disk without loading the entire file into memory
- **Live progress**: Shows bytes downloaded / total bytes (if Content-Length is available)
- **Resume support**: Automatically resumes interrupted downloads
- **Graceful error handling**: Network failures don't crash; partial downloads are preserved
- **Periodic state saves**: Progress is saved every 2 seconds or 1 MB, so even unexpected terminations retain recent progress
- **Signal handling**: Ctrl+C and SIGTERM save progress before exiting

## State File

The `ak.state.json` file tracks partial downloads in the current directory:

```json
{
  "https://example.com/file.zip": 1048576
}
```

- **Key**: The full URL being downloaded
- **Value**: Number of bytes already written to disk

When a download completes successfully, its entry is removed from the state file.

## Testing Resume Behavior

### Method 1: Testing Ctrl+C Resume (Recommended)

This is the most common real-world scenario:

```bash
# Start a large download
./ak fetch https://example.com/large-file.zip

# While it's downloading, press Ctrl+C
# You should see: "Received interrupt, saving progress..." and byte count

# Check that state was saved
cat ak.state.json
# Should show: {"https://example.com/large-file.zip": XXXXX}

# Run again - it should show "Resuming download from byte XXXXX..."
./ak fetch https://example.com/large-file.zip

# If server supports resume (206 response), it resumes from where you left off
# If server returns 200 OK, it restarts and shows "Server does not support resume"
```

### Method 2: Testing with `timeout` (Linux/macOS)

```bash
# Start a download, interrupt after 3 seconds
timeout 3s ./ak fetch https://example.com/large-file.zip

# Verify state was saved
cat ak.state.json

# Resume in a new terminal
./ak fetch https://example.com/large-file.zip
```

### Method 3: Manual State File Testing

```bash
# Create a test state file to simulate a partial download
echo '{"https://example.com/large-file.zip": 524288}' > ak.state.json

# Run ak - it will send "Range: bytes=524288-" header
./ak fetch https://example.com/large-file.zip

# Expected output:
# "Resuming download from byte 524288..."
# "Server supports resume (206 Partial Content)"
```

### Method 4: Using curl to Create Partial File

```bash
# Download first 1MB using curl (simulates interrupted download)
curl -r 0-1048575 -o large-file.zip https://example.com/large-file.zip

# Create matching state file
echo '{"https://example.com/large-file.zip": 1048576}' > ak.state.json

# Run ak to continue from 1MB
./ak fetch https://example.com/large-file.zip
```

## How Resume Works

1. On startup, `ak` reads `ak.state.json` to find any partial downloads
2. If a partial download exists for a URL, it sends `Range: bytes=N-` header
3. Server responds with:
   - **206 Partial Content**: Resume supported, continues from byte N
   - **200 OK**: Resume not supported, discards partial file and restarts
4. During download, state is saved every 2 seconds OR every 1 MB (whichever comes first)
5. On interrupt (Ctrl+C/SIGTERM), current progress is saved before exit
6. On network errors, current progress is saved for next run
7. On success, the URL entry is removed from the state file

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error (invalid URL, network failure, etc.) |
| 130 | Interrupted by user (Ctrl+C) - progress saved |

## Limitations

- Range requests work with servers that support HTTP/1.1 partial content
- Some CDNs and proxy servers may not support Range requests
- The state file must remain in the same directory as the downloaded file to resume
- SIGKILL (hard kill) cannot be caught - use Ctrl+C or SIGTERM for graceful interrupt
