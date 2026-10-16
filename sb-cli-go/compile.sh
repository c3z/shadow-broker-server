#!/bin/bash
set -e # Exit immediately if a command exits with a non-zero status.

# Check if Go is installed
if ! command -v go &> /dev/null
then
    echo "Go could not be found, please install it."
    exit 1
fi

# Get the directory of the script
DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

OUTPUT_NAME="sb"

echo "Compiling package into $OUTPUT_NAME..."

# Change to the script's directory to ensure relative paths work
cd "$DIR"

# Tidy dependencies (optional but good practice)
go mod tidy

# Build the whole package (main.go + tui.go + selftest.go). Building main.go
# alone silently drops the other files and the binary fails to compile.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
go build -ldflags "-X main.version=$VERSION" -o "$OUTPUT_NAME" .

if [ $? -eq 0 ]; then
    echo "Compilation successful! Executable created at $DIR/$OUTPUT_NAME"
    # Make executable (redundant as go build usually does this, but explicit)
    chmod +x "$OUTPUT_NAME"
else
    echo "Compilation failed."
    exit 1
fi 