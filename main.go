package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Core interface definitions
type (
	// Logger interface
	Logger interface {
		Info(format string, args ...interface{})
		Error(format string, args ...interface{})
	}

	// Downloader interface
	Downloader interface {
		Start(playlistURL string) error
		Close()
	}

	// SpaceAPI Twitter Space API interface
	SpaceAPI interface {
		GetStreamURL(spaceID string) (string, error)
		FetchSpaceData(spaceID string) (map[string]interface{}, error)
	}

	// HTTPClient interface
	HTTPClient interface {
		Do(req *http.Request) (*http.Response, error)
	}

	// FileWriter interface
	FileWriter interface {
		io.WriteCloser
		Name() string
	}
)

// Configuration related
type Config struct {
	ProxyURL    string
	Timeout     time.Duration
	BearerToken string
	Ct0         string
	AuthToken   string
}

// Constant definitions
const (
	PROXY_URL    = "http://127.0.0.1:7890"
	TIMEOUT      = 30 * time.Second
	BEARER_TOKEN = "AAAAAAAAAAAAAAAAAAAAANRILgAAAAAAnNwIzUejRCOuH5E6I8xnZz4puTs%3D1Zv7ttfk8LF81IUq16cHjhLTvJu4FA33AGWWjCpTnA"
	ct0          = "78af9a26b594b8139841001d6b6861aa2d4bd2e2c2702659b5d7e1a580be670f7e8ce22c133c470bcff0851a3d8257d6c85e5f66461eb173b66589ee76c7e95f73c1306f7c7dfcdcc4fdfd37fe604cd4"
	authToken    = "7fa4b40e84cf1db63c293781d2782f2e1b550ec7"

	MAX_RETRIES    = 10
	RETRY_INTERVAL = 5 * time.Second
	CHUNK_INTERVAL = 3 * time.Second
)

// Default configuration
var DefaultConfig = Config{
	ProxyURL:    PROXY_URL,
	Timeout:     TIMEOUT,
	BearerToken: BEARER_TOKEN,
	Ct0:         ct0,
	AuthToken:   authToken,
}

// ConsoleLogger implementation
type ConsoleLogger struct{}

func (l *ConsoleLogger) Info(format string, args ...interface{}) {
	fmt.Printf("%s "+format+"\n", append([]interface{}{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
}

func (l *ConsoleLogger) Error(format string, args ...interface{}) {
	fmt.Printf("%s [ERROR] "+format+"\n", append([]interface{}{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
}

// TwitterSpaceAPI implementation
type TwitterSpaceAPI struct {
	client HTTPClient
	config Config
	logger Logger
}

func NewTwitterSpaceAPI(client HTTPClient, config Config, logger Logger) SpaceAPI {
	return &TwitterSpaceAPI{
		client: client,
		config: config,
		logger: logger,
	}
}

// HLSDownloader implementation
type HLSDownloader struct {
	baseURL     string
	client      HTTPClient
	writer      FileWriter
	seenChunks  map[string]bool
	isCompleted bool
	config      Config
	logger      Logger
	stopChan    chan struct{}
}

func NewHLSDownloader(client HTTPClient, writer FileWriter, config Config, logger Logger) *HLSDownloader {
	return &HLSDownloader{
		client:     client,
		writer:     writer,
		seenChunks: make(map[string]bool),
		config:     config,
		logger:     logger,
		stopChan:   make(chan struct{}),
	}
}

// HTTPClientFactory
func CreateHTTPClient(config Config) (HTTPClient, error) {
	proxyURL, err := url.Parse(config.ProxyURL)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	return &http.Client{
		Transport: transport,
		Timeout:   config.Timeout,
	}, nil
}

// FileWriterFactory
func CreateFileWriter(spaceURL string) (FileWriter, error) {
	fileName := "recording.aac"
	if parts := strings.Split(spaceURL, "/spaces/"); len(parts) > 1 {
		// Extract Space ID and remove any extra path or parameters
		spaceID := strings.Split(strings.Split(parts[1], "/")[0], "?")[0]
		// Remove "peek" suffix
		spaceID = strings.ReplaceAll(spaceID, "-peek", "")
		fileName = spaceID + ".aac"
	}
	return os.Create(fileName)
}

// HLSDownloader methods
func (d *HLSDownloader) Start(playlistURL string) error {
	d.baseURL = playlistURL[:strings.LastIndex(playlistURL, "/")]
	d.logger.Info("Starting Twitter Space recording...")
	d.logger.Info("Output file: %s", d.writer.Name())

	retryCount := 0
	hasTriedReplay := false

	for !d.isCompleted {
		select {
		case <-d.stopChan:
			d.logger.Info("Download stopped")
			return nil
		default:
			chunks, err := d.getPlaylist(playlistURL)
			if err != nil {
				if strings.Contains(err.Error(), "No audio segments found") && !hasTriedReplay {
					playlistURL = d.switchToReplay(playlistURL)
					hasTriedReplay = true
					retryCount = 0
					continue
				}

				retryCount++
				if retryCount > MAX_RETRIES {
					return fmt.Errorf("Failed to get playlist, exceeded max retries: %v", err)
				}
				d.logger.Error("Failed to get playlist, retry %d/%d: %v", retryCount, MAX_RETRIES, err)
				time.Sleep(RETRY_INTERVAL)
				continue
			}

			retryCount = 0

			if err := d.processChunks(chunks); err != nil {
				return err
			}

			if !d.isCompleted {
				time.Sleep(CHUNK_INTERVAL)
			}
		}
	}

	d.logger.Info("Recording completed!")
	return nil
}

func (d *HLSDownloader) processChunks(chunks []string) error {
	for _, chunk := range chunks {
		select {
		case <-d.stopChan:
			return nil
		default:
			if !d.seenChunks[chunk] {
				chunkURL := fmt.Sprintf("%s/%s", d.baseURL, chunk)
				if err := d.downloadChunk(chunkURL); err != nil {
					if strings.Contains(err.Error(), "file already closed") {
						return nil
					}
					d.logger.Error("Failed to download chunk: %v", err)
					continue
				}
				d.seenChunks[chunk] = true
			}
		}
	}
	return nil
}

func (d *HLSDownloader) getPlaylist(playlistURL string) ([]string, error) {
	req, err := http.NewRequest("GET", playlistURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://twitter.com")
	req.Header.Set("Referer", "https://twitter.com/")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	content := string(body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error: %d, Body: %s", resp.StatusCode, content)
	}

	var chunks []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	var isChunk bool

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			isChunk = true
			continue
		}

		if isChunk && !strings.HasPrefix(line, "#") {
			chunks = append(chunks, line)
			isChunk = false
		}

		if strings.Contains(line, "#EXT-X-ENDLIST") {
			d.isCompleted = true
		}
	}

	if len(chunks) > 0 {
		firstChunk := chunks[0]
		if strings.HasPrefix(firstChunk, "http") {
			lastSlashIndex := strings.LastIndex(firstChunk, "/")
			if lastSlashIndex != -1 {
				d.baseURL = firstChunk[:lastSlashIndex]
				for i, chunk := range chunks {
					chunks[i] = filepath.Base(chunk)
				}
			}
		}
	}

	if len(chunks) == 0 {
		scanner = bufio.NewScanner(strings.NewReader(content))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasSuffix(line, ".m3u8") && !strings.Contains(line, "master_playlist") {
				playlistPath := line
				if !strings.HasPrefix(line, "http") {
					playlistPath = fmt.Sprintf("%s/%s", d.baseURL, line)
				}
				d.logger.Info("Found playlist file, attempting to fetch: %s", playlistPath)
				return d.getPlaylist(playlistPath)
			}
		}
		return nil, fmt.Errorf("No audio segments or valid playlist found")
	}

	d.logger.Info("Found %d audio segments", len(chunks))
	return chunks, nil
}

func (d *HLSDownloader) switchToReplay(playlistURL string) string {
	replayURL := strings.Replace(playlistURL, "type=live", "type=replay", 1)
	if strings.Contains(replayURL, "dynamic_playlist.m3u8") {
		replayURL = strings.Replace(replayURL, "dynamic_playlist.m3u8", "master_playlist.m3u8", 1)
	}

	d.logger.Info("Switching to replay mode...")
	d.logger.Info("New URL: %s", replayURL)

	d.baseURL = replayURL[:strings.LastIndex(replayURL, "/")]
	return replayURL
}

func (d *HLSDownloader) downloadChunk(chunkURL string) error {
	req, err := http.NewRequest("GET", chunkURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://twitter.com")
	req.Header.Set("Referer", "https://twitter.com/")

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP error: %d, Body: %s", resp.StatusCode, string(body))
	}

	_, err = io.Copy(d.writer, resp.Body)
	return err
}

func (d *HLSDownloader) Close() {
	select {
	case <-d.stopChan:
		// Channel already closed
	default:
		close(d.stopChan)
	}

	if d.writer != nil {
		d.writer.Close()
	}
}

// TwitterSpaceAPI implementation
func (api *TwitterSpaceAPI) GetStreamURL(spaceID string) (string, error) {
	space, err := api.FetchSpaceData(spaceID)
	if err != nil {
		return "", fmt.Errorf("Failed to fetch Space data: %v", err)
	}

	state, ok := space["state"].(string)
	if !ok {
		if status, ok := space["status"].(string); ok {
			state = status
		} else {
			return "", fmt.Errorf("Unable to find state information")
		}
	}

	isReplayAvailable := true
	if available, ok := space["is_space_available_for_replay"].(bool); ok {
		isReplayAvailable = available
	}

	if state == "Ended" && !isReplayAvailable {
		return "", fmt.Errorf("[ERROR] Space has ended, and no replay is available")
	}

	mediaKey, ok := space["media_key"].(string)
	if !ok {
		if key, ok := space["broadcast_id"].(string); ok {
			mediaKey = key
		} else {
			return "", fmt.Errorf("Unable to find media key")
		}
	}

	apiURL := fmt.Sprintf("https://twitter.com/i/api/1.1/live_video_stream/status/%s", mediaKey)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}

	api.setHeaders(req)

	resp, err := api.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Failed to read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error: %d, Body: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("Failed to parse JSON: %v", err)
	}

	source, ok := result["source"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("Invalid source format")
	}

	location, ok := source["location"].(string)
	if !ok {
		return "", fmt.Errorf("Invalid location format")
	}

	return location, nil
}

func (api *TwitterSpaceAPI) FetchSpaceData(spaceID string) (map[string]interface{}, error) {
	variables := fmt.Sprintf(`{
        "id": "%s",
        "isMetatagsQuery": true,
        "withSuperFollowsUserFields": true,
        "withDownvotePerspective": false,
        "withReactionsMetadata": false,
        "withReactionsPerspective": false,
        "withSuperFollowsTweetFields": true,
        "withReplays": true
    }`, spaceID)

	features := `{
        "spaces_2022_h2_clipping": true,
        "spaces_2022_h2_spaces_communities": true,
        "responsive_web_twitter_blue_verified_badge_is_enabled": true,
        "verified_phone_label_enabled": false,
        "view_counts_public_visibility_enabled": true,
        "longform_notetweets_consumption_enabled": false,
        "tweetypie_unmention_optimization_enabled": true,
        "responsive_web_uc_gql_enabled": true,
        "vibe_api_enabled": true,
        "responsive_web_edit_tweet_api_enabled": true,
        "graphql_is_translatable_rweb_tweet_is_translatable_enabled": true,
        "view_counts_everywhere_api_enabled": true,
        "standardized_nudges_misinfo": true,
        "tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": false,
        "responsive_web_graphql_timeline_navigation_enabled": true,
        "interactive_text_enabled": true,
        "responsive_web_text_conversations_enabled": false,
        "responsive_web_enhance_cards_enabled": false
    }`

	apiURL := "https://twitter.com/i/api/graphql/xjTKygiBMpX44KU8ywLohQ/AudioSpaceById"
	params := url.Values{}
	params.Add("variables", variables)
	params.Add("features", features)

	req, err := http.NewRequest("GET", apiURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}

	api.setHeaders(req)

	resp, err := api.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to read response: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("Failed to parse JSON: %v", err)
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("No data field in response")
	}

	audioSpace, ok := data["audioSpace"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("No audioSpace field in response")
	}

	metadata, ok := audioSpace["metadata"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Unable to find metadata field")
	}

	return metadata, nil
}

func (api *TwitterSpaceAPI) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+api.config.BearerToken)
	req.Header.Set("x-csrf-token", api.config.Ct0)
	req.Header.Set("Cookie", fmt.Sprintf("auth_token=%s; ct0=%s", api.config.AuthToken, api.config.Ct0))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Twitter-Active-User", "yes")
	req.Header.Set("X-Twitter-Client-Language", "en")
}

// Helper function
func extractSpaceID(spaceURL string) string {
	if strings.Contains(spaceURL, "/spaces/") {
		parts := strings.Split(spaceURL, "/spaces/")
		if len(parts) > 1 {
			return strings.Split(strings.Split(parts[1], "/")[0], "?")[0]
		}
	}
	return ""
}

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: space-download.exe <Space URL>")
		os.Exit(1)
	}

	spaceURL := os.Args[1]
	spaceID := extractSpaceID(spaceURL)

	if spaceID == "" {
		fmt.Println("[ERROR] Unable to extract Space ID from URL")
		os.Exit(1)
	}

	// Create dependencies
	logger := &ConsoleLogger{}
	client, err := CreateHTTPClient(DefaultConfig)
	if err != nil {
		logger.Error("Failed to create HTTP client: %v", err)
		os.Exit(1)
	}

	// Create API client
	api := NewTwitterSpaceAPI(client, DefaultConfig, logger)

	// Get stream URL
	playlistURL, err := api.GetStreamURL(spaceID)
	if err != nil {
		logger.Error("Failed to get stream URL: %v", err)
		os.Exit(1)
	}

	// Create file writer
	writer, err := CreateFileWriter(spaceURL)
	if err != nil {
		logger.Error("Failed to create file: %v", err)
		os.Exit(1)
	}
	defer writer.Close()

	// Create downloader
	downloader := NewHLSDownloader(client, writer, DefaultConfig, logger)
	defer downloader.Close()

	// Start download
	if err := downloader.Start(playlistURL); err != nil {
		logger.Error("Download failed: %v", err)
		os.Exit(1)
	}
}
