package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/schollz/progressbar/v3"
)

// Config holds application configuration
type Config struct {
	AppID    int
	AppHash  string
	Phone    string
	FilePath string
	TargetID string // Username or chat ID to send the file to
}

func main() {
	// Parse command-line flags
	appID := flag.Int("api-id", 0, "Telegram API ID")
	appHash := flag.String("api-hash", "", "Telegram API Hash")
	phone := flag.String("phone", "", "Phone number in international format")
	filePath := flag.String("file", "", "Path to the file to upload")
	fileURL := flag.String("url", "", "URL of the file to download and upload")
	targetID := flag.String("target", "me", "Target username or chat ID (default: 'me' for Saved Messages)")
	flag.Parse()

	// Validate inputs
	if *appID == 0 || *appHash == "" {
		log.Fatal("API ID and API Hash are required")
	}
	if *filePath == "" && *fileURL == "" {
		log.Fatal("Either file path or URL is required")
	}
	if *phone == "" {
		log.Fatal("Phone number is required")
	}

	// If URL is provided, download the file
	finalFilePath := *filePath
	if *fileURL != "" {
		fmt.Println("Downloading file from URL...")
		tmpPath, err := downloadFileFromURL(*fileURL)
		if err != nil {
			log.Fatalf("Failed to download file: %v", err)
		}
		finalFilePath = tmpPath
		defer os.Remove(tmpPath) // Clean up temp file after upload
	}

	// Create config
	config := &Config{
		AppID:    *appID,
		AppHash:  *appHash,
		Phone:    *phone,
		FilePath: finalFilePath,
		TargetID: *targetID,
	}

	// Run the application
	if err := run(config); err != nil {
		log.Fatal(err)
	}
}

// downloadFileFromURL downloads a file from the given URL and returns the local file path
func downloadFileFromURL(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}

	// Try to get filename from URL or Content-Disposition
	filename := filepath.Base(resp.Request.URL.Path)
	if filename == "" || filename == "/" {
		filename = "downloaded_file"
	}

	tmpFile, err := os.CreateTemp("", filename)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	// Create progress bar for download
	fmt.Printf("Downloading %s...\n", filename)
	bar := progressbar.NewOptions64(
		resp.ContentLength,
		progressbar.OptionSetDescription("Downloading"),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(30),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() { fmt.Println() }),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionSetElapsedTime(true),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionFullWidth(),
	)

	// Copy the body to the file with progress bar
	_, err = io.Copy(io.MultiWriter(tmpFile, bar), resp.Body)
	if err != nil {
		return "", err
	}

	return tmpFile.Name(), nil
}

func run(config *Config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Setup session storage
	sessionDir := filepath.Join(".", "sessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}
	sessionPath := filepath.Join(sessionDir, fmt.Sprintf("%s.session", strings.ReplaceAll(config.Phone, "+", "")))
	sessStorage := &session.FileStorage{
		Path: sessionPath,
	}

	// Initialize client
	client := telegram.NewClient(config.AppID, config.AppHash, telegram.Options{
		SessionStorage: sessStorage,
	})

	// Start the client and handle authentication
	return client.Run(ctx, func(ctx context.Context) error {
		// Check if we're logged in
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("failed to get auth status: %w", err)
		}

		// Authenticate if needed
		if !status.Authorized {
			log.Println("Starting authentication flow...")
			flow := auth.NewFlow(
				termAuth{phone: config.Phone},
				auth.SendCodeOptions{},
			)
			if err := client.Auth().IfNecessary(ctx, flow); err != nil {
				return fmt.Errorf("authentication failed: %w", err)
			}
		}
		log.Println("Successfully authenticated!")

		// Upload the file
		return uploadFile(ctx, client, config)
	})
}

func uploadFile(ctx context.Context, client *telegram.Client, config *Config) error {
	// Check if file exists
	fileInfo, err := os.Stat(config.FilePath)
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}

	fileSize := fileInfo.Size()

	// Log info
	fmt.Printf("Preparing to upload file: %s (%.2f MB)\n", config.FilePath, float64(fileSize)/(1024*1024))

	// Create Telegram API client
	api := client.API()

	// Create uploader with larger part size for big files
	// Use 512KB parts for better performance with large files
	u := uploader.NewUploader(api).WithPartSize(512 * 1024)

	// Open the file
	file, err := os.Open(config.FilePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Create progress bar
	bar := progressbar.NewOptions64(
		fileSize,
		progressbar.OptionSetDescription("Uploading"),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(30),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() { fmt.Println() }),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionSetElapsedTime(true),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionFullWidth(),
	)

	// Create a wrapper around the file to track progress
	reader := &progressReader{
		Reader: file,
		bar:    bar,
	}

	// Start time for calculating upload speed
	startTime := time.Now()

	// Create a ticker for updating the speed display
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Start a goroutine to update the speed display
	done := make(chan bool)

	go func() {
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					// Using State() instead of Current()
					state := bar.State()
					speed := float64(state.CurrentBytes) / elapsed / (1024 * 1024) // MB/s
					bar.Describe(fmt.Sprintf("Uploading (%.2f MB/s)", speed))
				}
			case <-done:
				return
			}
		}
	}()

	// Upload the file (using the correct method and parameters)
	fileName := filepath.Base(config.FilePath)
	upload, err := u.Upload(ctx, uploader.NewUpload(fileName, reader, fileSize))

	// Signal the speed update goroutine to stop
	close(done)

	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	fmt.Printf("\nUpload completed successfully in %s!\n", time.Since(startTime).Round(time.Second))

	// Get mime type based on file extension
	mimeType := getMimeType(fileName)

	// Determine target user or chat
	var target tg.InputPeerClass
	// For simplicity, we'll use "Saved Messages" (self) as the target
	target = &tg.InputPeerSelf{}
	fmt.Println("Sending to Saved Messages...")

	// Prepare media
	var media tg.InputMediaClass
	// Determine type of file and use appropriate media type
	ext := strings.ToLower(filepath.Ext(fileName))
	switch {
	case isImageFile(ext):
		media = &tg.InputMediaUploadedPhoto{
			File: upload,
		}
		fmt.Println("Processing as photo")
	case isVideoFile(ext):
		media = &tg.InputMediaUploadedDocument{
			File:     upload,
			MimeType: mimeType,
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: fileName},
				&tg.DocumentAttributeVideo{
					SupportsStreaming: true,
				},
			},
		}
		fmt.Println("Processing as video")
	default:
		// Upload as generic document
		media = &tg.InputMediaUploadedDocument{
			File:     upload,
			MimeType: mimeType,
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: fileName},
			},
		}
		fmt.Println("Processing as document")
	}

	// Generate a random ID for the message
	randomID, err := generateRandomID()
	if err != nil {
		return fmt.Errorf("failed to generate random ID: %w", err)
	}

	// Send the message with the uploaded media
	fmt.Println("Finalizing file in Telegram...")
	_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     target,
		Media:    media,
		Message:  fmt.Sprintf("Uploaded file: %s", fileName),
		RandomID: randomID, // Add the random ID here
	})
	if err != nil {
		return fmt.Errorf("failed to send media: %w", err)
	}
	fmt.Println("✅ File successfully sent to Saved Messages!")
	fmt.Println("Open your Telegram app and check your Saved Messages to access the file.")
	return nil
}

// generateRandomID generates a random int64 to use as message ID
func generateRandomID() (int64, error) {
	var id int64
	err := binary.Read(rand.Reader, binary.LittleEndian, &id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// progressReader is an io.Reader that updates a progress bar as data is read
type progressReader struct {
	io.Reader
	bar *progressbar.ProgressBar
}

// Read implements io.Reader
func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.Reader.Read(p)
	if n > 0 {
		pr.bar.Add(n)
	}
	return
}

// termAuth implements auth.UserAuthenticator interface for terminal authentication
type termAuth struct {
	phone string
}

func (a termAuth) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (a termAuth) Password(_ context.Context) (string, error) {
	fmt.Print("Enter your 2FA password: ")
	password, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(password), nil
}

func (a termAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter the authentication code sent to your Telegram: ")
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(code), nil
}

// AcceptTermsOfService implements the required method for auth.UserAuthenticator
func (a termAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	fmt.Println("Terms of Service:")
	fmt.Println(tos.Text)
	fmt.Print("Do you accept the Terms of Service? (y/n): ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return fmt.Errorf("terms of service not accepted")
	}
	return nil
}

// SignUp implements auth.UserAuthenticator interface
func (a termAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	var info auth.UserInfo
	fmt.Print("Enter your first name: ")
	firstName, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return info, err
	}
	info.FirstName = strings.TrimSpace(firstName)
	fmt.Print("Enter your last name (optional): ")
	lastName, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return info, err
	}
	info.LastName = strings.TrimSpace(lastName)
	return info, nil
}

// Helper functions for file types
func isImageFile(ext string) bool {
	imageExts := []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"}
	for _, imgExt := range imageExts {
		if ext == imgExt {
			return true
		}
	}
	return false
}

func isVideoFile(ext string) bool {
	videoExts := []string{".mp4", ".mov", ".avi", ".mkv", ".webm", ".flv", ".3gp"}
	for _, vidExt := range videoExts {
		if ext == vidExt {
			return true
		}
	}
	return false
}

func getMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".mkv":
		return "video/x-matroska"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}
