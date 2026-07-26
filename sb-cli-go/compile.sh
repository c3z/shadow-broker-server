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
MAIN_GO="main.go"

echo "Compiling $MAIN_GO into $OUTPUT_NAME..."

# Change to the script's directory to ensure relative paths work
cd "$DIR"

# Tidy dependencies (optional but good practice)
go mod tidy

# Build the Go program
# -o specifies the output file name
# We pass the main go file to build
go build -o "$OUTPUT_NAME" "$MAIN_GO"

if [ $? -eq 0 ]; then
    echo "Compilation successful! Executable created at $DIR/$OUTPUT_NAME"
    # Make executable (redundant as go build usually does this, but explicit)
    chmod +x "$OUTPUT_NAME"
else
    echo "Compilation failed."
    exit 1
fi 