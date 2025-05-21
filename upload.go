package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

func main() {
	// Parse command-line flags
	filePath := flag.String("file", "", "Path to the file to upload")
	targetID := flag.String("target", "me", "Target username or chat ID (default: 'me' for Saved Messages)")
	flag.Parse()

	// Check if file is provided
	if *filePath == "" {
		log.Fatal("File path is required. Use --file flag.")
	}

	// Make sure the file exists
	if _, err := os.Stat(*filePath); os.IsNotExist(err) {
		log.Fatalf("File does not exist: %s", *filePath)
	}

	// Load configuration
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}

	// Get required config values
	apiID := viper.GetInt("API_ID")
	apiHash := viper.GetString("API_HASH")
	phone := viper.GetString("PHONE")

	if apiID == 0 || apiHash == "" || phone == "" {
		log.Fatal("API_ID, API_HASH, and PHONE must be set in config.yaml")
	}

	// Get absolute path of file
	absPath, err := filepath.Abs(*filePath)
	if err != nil {
		log.Fatalf("Failed to get absolute path: %v", err)
	}

	// Generate command
	args := []string{
		"-api-id", fmt.Sprintf("%d", apiID),
		"-api-hash", apiHash,
		"-phone", phone,
		"-file", absPath,
		"-target", *targetID,
	}

	// Get the path to the main binary
	mainBinary, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}

	// If this is the upload.go binary, use "go run main.go" instead
	if strings.Contains(filepath.Base(mainBinary), "upload") {
		cmd := exec.Command("go", append([]string{"run", "main.go"}, args...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Fatalf("Command failed: %v", err)
		}
	} else {
		// We're already running the main binary
		log.Println("Starting upload...")
		// Call main function directly
		os.Args = append([]string{os.Args[0]}, args...)
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		flag.Parse()
		// Now you can call the main program's logic
	}
}
