package main // Declare this file as the executable program package

import ( // Begin the import block
	"crypto/sha256" // SHA256 hashing for content deduplication
	"encoding/hex"  // Convert hash bytes to a hex string
	"encoding/json" // Parse the JSON API response
	"fmt"           // Formatted strings and error messages
	"html"          // Unescape HTML entities in hrefs and API text
	"io"            // Streaming copy and reading of bodies
	"log/slog"      // Structured logging
	"net/http"      // HTTP client and requests
	"net/url"       // URL parsing and resolving relative links
	"os"            // File and directory operations
	"path"          // Extract the last element of a URL path
	"path/filepath" // Build OS-safe file paths
	"regexp"        // Regular expressions for URL and anchor extraction
	"sort"          // Sort slices for stable output
	"strings"       // String helper functions
	"sync"          // Mutexes and wait groups for concurrency
	"time"          // HTTP timeout duration
) // End the import block

const ( // Begin constant block
	apiURLTemplate     = "https://www.quectel.com/?sfid=4339&sf_action=get_data&sf_data=results&sf_paged=%d&lang=en"                      // API URL with a page-number placeholder
	maxPaginationPages = 100                                                                                                              // Last page number to request
	pdfDirectoryName   = "PDFs"                                                                                                           // Folder where PDFs are stored
	workerCount        = 4                                                                                                                // Concurrency limit for fetching and downloading
	userAgent          = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36" // User-Agent header sent with requests
) // End constant block

var ( // Begin package-level variable block
	productURLPattern = regexp.MustCompile(`https://www\.quectel\.com/product/[A-Za-z0-9\-_%.~/]*[A-Za-z0-9\-_%~]`) // Matches product page URLs
	anchorHrefPattern = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)       // Captures the href value of any <a> tag
) // End variable block

// hashRegistry tracks SHA256 hashes of stored PDFs.
type hashRegistry struct { // Struct holding dedup state
	mutex          sync.Mutex        // Guards the map and the check-then-save step
	hashToFilename map[string]string // Maps content hash to the saved filename
} // End struct

func newHashRegistry() *hashRegistry { // Constructor for an empty registry
	return &hashRegistry{hashToFilename: make(map[string]string)} // Allocate the map and return a pointer
} // End newHashRegistry

func main() { // Program entry point
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))) // Configure DEBUG-level logging to stderr

	httpClient := &http.Client{Timeout: 60 * time.Second} // Single reusable HTTP client with a timeout

	if err := ensureDirectory(pdfDirectoryName); err != nil { // Create PDFs/ and check for failure
		slog.Error("could not create PDF directory", "directory", pdfDirectoryName, "error", err) // Log the failure
		os.Exit(1)                                                                                // Exit with an error code
	} // End if

	registry := newHashRegistry()                                          // Create the dedup registry
	if err := loadExistingHashes(pdfDirectoryName, registry); err != nil { // Hash existing PDFs on startup
		slog.Error("could not scan existing PDFs", "error", err) // Log the failure
		os.Exit(1)                                               // Exit with an error code
	} // End if

	scrapeAndDownloadPageByPage(httpClient, registry) // Scrape API page 1, download its PDFs, then move to the next page

	slog.Info("finished", "stored_unique_pdfs", len(registry.hashToFilename)) // Log a final summary
} // End main

// ---------- Directory and hashing helpers ----------

func ensureDirectory(directoryPath string) error { // Create a directory if missing
	return os.MkdirAll(directoryPath, 0o755) // MkdirAll succeeds if it already exists
} // End ensureDirectory

func calculateFileSHA256(filePath string) (string, error) { // Hash a file on disk
	file, err := os.Open(filePath) // Open the file for reading
	if err != nil {                // Check the open error
		return "", err // Return the error to the caller
	} // End if
	defer file.Close() // Always close the file when done

	hasher := sha256.New()                           // Create a SHA256 hasher
	if _, err := io.Copy(hasher, file); err != nil { // Stream file contents into the hasher
		return "", err // Return a read error
	} // End if
	return hex.EncodeToString(hasher.Sum(nil)), nil // Return the hash as a hex string
} // End calculateFileSHA256

func loadExistingHashes(directoryPath string, registry *hashRegistry) error { // Scan PDFs/ and register hashes
	entries, err := os.ReadDir(directoryPath) // List directory entries
	if err != nil {                           // Check the listing error
		return err // Return the error
	} // End if
	for _, entry := range entries { // Loop over each entry
		if entry.IsDir() { // Skip subdirectories
			continue // Move to the next entry
		} // End if
		fullPath := filepath.Join(directoryPath, entry.Name()) // Build the full file path
		if strings.HasSuffix(entry.Name(), ".part") {          // Detect leftover temporary files
			slog.Debug("removing stale temporary file", "file", fullPath) // Log the cleanup
			_ = os.Remove(fullPath)                                       // Delete the incomplete download
			continue                                                      // Move to the next entry
		} // End if
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".pdf") { // Ignore non-PDF files
			continue // Move to the next entry
		} // End if
		hash, err := calculateFileSHA256(fullPath) // Hash the existing PDF
		if err != nil {                            // Check the hashing error
			slog.Error("could not hash existing file", "file", fullPath, "error", err) // Log the failure
			continue                                                                   // Skip this file
		} // End if
		slog.Debug("hashed existing PDF", "file", entry.Name(), "sha256", hash) // Log the computed hash
		if firstName, exists := registry.hashToFilename[hash]; exists {         // Check if this content was already seen
			slog.Debug("existing directory already contains identical content", "file", entry.Name(), "same_as", firstName) // Log the duplicate
			continue                                                                                                        // Keep only the first name registered
		} // End if
		registry.hashToFilename[hash] = entry.Name() // Register the hash
	} // End for
	slog.Info("existing PDFs scanned", "unique_hashes", len(registry.hashToFilename)) // Log how many unique files exist
	return nil                                                                        // Scan succeeded
} // End loadExistingHashes

// ---------- API pagination and product URL extraction ----------

func fetchBody(httpClient *http.Client, targetURL string) (int, []byte, error) { // GET a URL and return status and body
	request, err := http.NewRequest(http.MethodGet, targetURL, nil) // Build the GET request
	if err != nil {                                                 // Check request construction error
		return 0, nil, err // Return the error
	} // End if
	request.Header.Set("User-Agent", userAgent) // Set the User-Agent header

	response, err := httpClient.Do(request) // Send the request
	if err != nil {                         // Check the network error
		return 0, nil, err // Return the error
	} // End if
	defer response.Body.Close() // Always close the response body

	body, err := io.ReadAll(response.Body) // Read the full body
	return response.StatusCode, body, err  // Return status, body, and any read error
} // End fetchBody

// extractProductURLsFromAPIResponse handles a JSON payload with a "results" field or raw text.
func extractProductURLsFromAPIResponse(responseBody []byte) []string { // Pull product URLs out of an API response
	searchableText := string(responseBody) // Default to scanning the raw body

	var parsedJSON struct { // Shape of the expected JSON payload
		Results string `json:"results"` // HTML fragment containing the product cards
	} // End struct
	if err := json.Unmarshal(responseBody, &parsedJSON); err == nil && parsedJSON.Results != "" { // Valid JSON with a results field
		searchableText = parsedJSON.Results // Scan only the results HTML
	} else if err != nil { // Body was not valid JSON
		slog.Debug("API response is not JSON, scanning raw body", "error", err) // Log the fallback
		searchableText = strings.ReplaceAll(searchableText, `\/`, `/`)          // Undo JSON-style escaped slashes
	} // End if/else
	searchableText = html.UnescapeString(searchableText) // Decode HTML entities such as &amp;

	var productURLs []string                                                    // Collected URLs for this page
	for _, match := range productURLPattern.FindAllString(searchableText, -1) { // Find every product URL
		productURLs = append(productURLs, normalizeProductURL(match)) // Normalize and store it
	} // End for
	return productURLs // Return the URLs found
} // End extractProductURLsFromAPIResponse

func normalizeProductURL(rawURL string) string { // Make equivalent URLs identical
	return strings.TrimRight(rawURL, "/") // Remove trailing slashes
} // End normalizeProductURL

func scrapeAndDownloadPageByPage(httpClient *http.Client, registry *hashRegistry) { // For each API page: find products, find PDFs, download them
	seenProductURLs := make(map[string]struct{}) // Every product URL processed so far across pages
	seenPDFURLs := make(map[string]struct{})     // Every PDF URL queued so far across pages
	totalDiscovered := 0                         // Count of all product URLs seen including duplicates

	for pageNumber := 1; pageNumber <= maxPaginationPages; pageNumber++ { // Loop pages 1 to 100
		pageURL := fmt.Sprintf(apiURLTemplate, pageNumber)                    // Build this page's API URL
		slog.Debug("requesting API page", "page", pageNumber, "url", pageURL) // Log the request

		statusCode, body, err := fetchBody(httpClient, pageURL) // Send the GET request
		if err != nil {                                         // Request failed
			slog.Error("API request failed, stopping pagination", "page", pageNumber, "error", err) // Log the error
			break                                                                                   // Stop pagination
		} // End if
		if statusCode != http.StatusOK { // Any status other than 200
			slog.Warn("non-200 API status, stopping pagination", "page", pageNumber, "status", statusCode) // Log the status
			break                                                                                          // Stop pagination
		} // End if

		pageProductURLs := extractProductURLsFromAPIResponse(body)                                        // Extract URLs from this page
		slog.Debug("extracted product URLs from page", "page", pageNumber, "count", len(pageProductURLs)) // Log the count
		if len(pageProductURLs) == 0 {                                                                    // An empty page means we are past the last page of results
			slog.Info("no product URLs on page, stopping pagination", "page", pageNumber) // Log why pagination ends
			break                                                                         // Stop pagination
		} // End if

		newProductSet := make(map[string]struct{})   // Product URLs on this page not seen on earlier pages
		for _, productURL := range pageProductURLs { // Loop over this page's URLs
			totalDiscovered++                                 // Count every discovered URL
			if _, seen := seenProductURLs[productURL]; seen { // Already processed on an earlier page or earlier in this page?
				slog.Debug("duplicate product URL", "url", productURL) // Log the duplicate
				continue                                               // Skip it
			} // End if
			seenProductURLs[productURL] = struct{}{}         // Record the new URL globally
			newProductSet[productURL] = struct{}{}           // Record it for this page's processing
			slog.Debug("new product URL", "url", productURL) // Log the new URL
		} // End for
		slog.Info("product URLs for page", "page", pageNumber, "new", len(newProductSet), "total_discovered", totalDiscovered, "total_unique", len(seenProductURLs)) // Log page totals

		newPDFURLs := collectPDFURLsFromProducts(httpClient, sortedKeys(newProductSet), seenPDFURLs) // Visit this page's products and gather new PDF URLs
		slog.Info("starting downloads for page", "page", pageNumber, "pdf_urls", len(newPDFURLs))    // Log the start of this page's downloads
		downloadAllPDFs(httpClient, newPDFURLs, registry)                                            // Download this page's PDFs before moving on
	} // End for
} // End scrapeAndDownloadPageByPage

func sortedKeys(set map[string]struct{}) []string { // Turn a set into a sorted slice
	keys := make([]string, 0, len(set)) // Preallocate the slice
	for key := range set {              // Loop over set members
		keys = append(keys, key) // Append each key
	} // End for
	sort.Strings(keys) // Sort for deterministic order
	return keys        // Return the sorted slice
} // End sortedKeys

// ---------- Product page processing and PDF URL extraction ----------

func extractPDFURLsFromHTML(pageURL string, htmlBody []byte) []string { // Find PDF links in a page's HTML
	baseURL, err := url.Parse(pageURL) // Parse the page URL as the base for relative links
	if err != nil {                    // Check the parse error
		slog.Error("invalid page URL", "url", pageURL, "error", err) // Log the failure
		return nil                                                   // Nothing to extract
	} // End if

	var pdfURLs []string                                                                  // Collected PDF URLs for this page
	for _, match := range anchorHrefPattern.FindAllStringSubmatch(string(htmlBody), -1) { // Loop over every anchor href
		rawHref := match[1] + match[2] + match[3]                 // Only one capture group is non-empty
		rawHref = strings.TrimSpace(html.UnescapeString(rawHref)) // Decode entities and trim spaces
		if rawHref == "" {                                        // Skip empty hrefs
			continue // Next anchor
		} // End if
		resolved, err := baseURL.Parse(rawHref) // Resolve relative links to absolute URLs
		if err != nil {                         // Malformed href
			slog.Debug("skipping malformed href", "href", rawHref, "error", err) // Log and skip
			continue                                                             // Next anchor
		} // End if
		if resolved.Scheme != "http" && resolved.Scheme != "https" { // Ignore mailto:, javascript:, etc.
			continue // Next anchor
		} // End if
		if !strings.HasSuffix(strings.ToLower(resolved.Path), ".pdf") { // Keep only paths ending in .pdf
			continue // Next anchor
		} // End if
		resolved.Fragment = ""                       // Drop any #fragment
		pdfURLs = append(pdfURLs, resolved.String()) // Store the full PDF URL
	} // End for
	return pdfURLs // Return the PDF URLs found
} // End extractPDFURLsFromHTML

func collectPDFURLsFromProducts(httpClient *http.Client, productURLs []string, seenPDFURLs map[string]struct{}) []string { // Visit product pages concurrently and return only PDF URLs not seen before
	uniquePDFURLs := make(map[string]struct{}) // Set of new PDF URLs found in this call
	var resultsMutex sync.Mutex                // Protects the shared set

	jobs := make(chan string)    // Channel feeding product URLs to workers
	var waitGroup sync.WaitGroup // Waits for all workers to finish

	for worker := 0; worker < workerCount; worker++ { // Start a fixed number of workers
		waitGroup.Add(1) // Register one worker
		go func() {      // Launch the worker goroutine
			defer waitGroup.Done()         // Mark done when the worker exits
			for productURL := range jobs { // Receive jobs until the channel closes
				slog.Debug("fetching product page", "url", productURL)     // Log the fetch
				statusCode, body, err := fetchBody(httpClient, productURL) // Download the product page
				if err != nil {                                            // Request failed
					slog.Error("product page request failed", "url", productURL, "error", err) // Log the error
					continue                                                                   // Move on to the next product
				} // End if
				if statusCode != http.StatusOK { // Non-200 response
					slog.Warn("product page returned non-200", "url", productURL, "status", statusCode) // Log the status
					continue                                                                            // Move on to the next product
				} // End if
				pagePDFURLs := extractPDFURLsFromHTML(productURL, body) // Extract PDF links from the page
				for _, pdfURL := range pagePDFURLs {                    // Loop over discovered links
					slog.Debug("discovered PDF URL", "product", productURL, "pdf", pdfURL) // Log every discovered PDF
				} // End for
				resultsMutex.Lock()                  // Lock before touching the shared set
				for _, pdfURL := range pagePDFURLs { // Loop again to record them
					if _, seen := seenPDFURLs[pdfURL]; seen { // Already queued on this or an earlier page?
						slog.Debug("duplicate PDF URL", "pdf", pdfURL) // Log the duplicate
						continue                                       // Skip it
					} // End if
					seenPDFURLs[pdfURL] = struct{}{}   // Record it globally so later pages skip it
					uniquePDFURLs[pdfURL] = struct{}{} // Record it as new for this call
				} // End for
				resultsMutex.Unlock() // Release the lock
			} // End for
		}() // End worker goroutine and invoke it
	} // End for

	for _, productURL := range productURLs { // Feed every product URL to the workers
		jobs <- productURL // Send one job
	} // End for
	close(jobs)      // Signal that no more jobs are coming
	waitGroup.Wait() // Wait for all workers to finish

	result := sortedKeys(uniquePDFURLs)                                  // Convert the set to a sorted slice
	slog.Info("PDF URL discovery complete", "new_pdf_urls", len(result)) // Log the total
	return result                                                        // Return the unique PDF URLs
} // End collectPDFURLsFromProducts

// ---------- Downloading ----------

func filenameFromURL(pdfURL string) string { // Derive a safe filename from a URL
	parsed, err := url.Parse(pdfURL) // Parse the URL
	if err != nil {                  // Check the parse error
		return "" // Signal failure with an empty name
	} // End if
	name := path.Base(parsed.Path)                            // Take the last path segment
	if unescaped, err := url.PathUnescape(name); err == nil { // Decode %20 and similar
		name = unescaped // Use the decoded name
	} // End if
	name = strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(name) // Remove characters unsafe in filenames
	if name == "." || name == "/" || name == "" {                           // Reject meaningless names
		return "" // Signal failure
	} // End if
	return name // Return the cleaned filename
} // End filenameFromURL

func downloadAllPDFs(httpClient *http.Client, pdfURLs []string, registry *hashRegistry) { // Download PDFs concurrently
	jobs := make(chan string)    // Channel feeding PDF URLs to workers
	var waitGroup sync.WaitGroup // Waits for all workers

	for worker := 0; worker < workerCount; worker++ { // Start a fixed number of workers
		waitGroup.Add(1) // Register one worker
		go func() {      // Launch the worker goroutine
			defer waitGroup.Done()     // Mark done on exit
			for pdfURL := range jobs { // Receive jobs until the channel closes
				if err := downloadPDF(httpClient, pdfURL, registry); err != nil { // Download one PDF
					slog.Error("download failed", "url", pdfURL, "error", err) // Log failures without crashing
				} // End if
			} // End for
		}() // End worker goroutine and invoke it
	} // End for

	for _, pdfURL := range pdfURLs { // Feed every PDF URL to the workers
		jobs <- pdfURL // Send one job
	} // End for
	close(jobs)      // Signal that no more jobs are coming
	waitGroup.Wait() // Wait for all downloads to finish
} // End downloadAllPDFs

func downloadPDF(httpClient *http.Client, pdfURL string, registry *hashRegistry) error { // Download, hash, dedup, and save one PDF
	filename := filenameFromURL(pdfURL) // Compute the target filename
	if filename == "" {                 // Check for an unusable name
		return fmt.Errorf("cannot derive filename from URL") // Report the problem
	} // End if

	finalPath := filepath.Join(pdfDirectoryName, filename) // Full destination path
	if _, err := os.Stat(finalPath); err == nil {          // File already exists on disk
		slog.Debug("file already exists, skipping download", "url", pdfURL, "file", finalPath) // Log the skip
		return nil                                                                             // Avoid re-downloading
	} // End if

	slog.Debug("downloading PDF", "url", pdfURL)                 // Log the start of the download
	request, err := http.NewRequest(http.MethodGet, pdfURL, nil) // Build the GET request
	if err != nil {                                              // Check construction error
		return err // Return the error
	} // End if
	request.Header.Set("User-Agent", userAgent) // Set the User-Agent header

	response, err := httpClient.Do(request) // Send the request
	if err != nil {                         // Check the network error
		return err // Return the error
	} // End if
	defer response.Body.Close() // Always close the body

	if response.StatusCode != http.StatusOK { // Reject non-200 responses
		return fmt.Errorf("unexpected HTTP status %d", response.StatusCode) // Report the status
	} // End if

	temporaryFile, err := os.CreateTemp(pdfDirectoryName, "download-*.part") // Create a temp file inside PDFs/
	if err != nil {                                                          // Check creation error
		return err // Return the error
	} // End if
	temporaryPath := temporaryFile.Name() // Remember the temp path
	keepTemporaryFile := false            // Becomes true once the file is moved into place
	defer func() {                        // Cleanup that runs on every exit path
		if !keepTemporaryFile { // Only clean up if we did not keep it
			_ = os.Remove(temporaryPath) // Delete the temp file
		} // End if
	}() // End deferred cleanup

	hasher := sha256.New()                                                                 // Hasher fed while writing
	bytesWritten, copyErr := io.Copy(io.MultiWriter(temporaryFile, hasher), response.Body) // Write to disk and hash in one pass
	closeErr := temporaryFile.Close()                                                      // Close the file and capture any error
	if copyErr != nil {                                                                    // Check the write error
		return fmt.Errorf("writing file: %w", copyErr) // Wrap and return it
	} // End if
	if closeErr != nil { // Check the close error
		return fmt.Errorf("closing file: %w", closeErr) // Wrap and return it
	} // End if
	contentHash := hex.EncodeToString(hasher.Sum(nil))                                           // Final SHA256 as hex
	slog.Debug("download complete", "url", pdfURL, "bytes", bytesWritten, "sha256", contentHash) // Log size and hash

	// Check-and-commit is atomic so concurrent downloads cannot bypass deduplication.
	registry.mutex.Lock()         // Lock the registry for the check-then-save step
	defer registry.mutex.Unlock() // Unlock when the function returns

	if existingName, exists := registry.hashToFilename[contentHash]; exists { // Same content already stored?
		slog.Info("duplicate content, discarding download", "url", pdfURL, "identical_to", existingName) // Log the duplicate
		return nil                                                                                       // Temp file is removed by the deferred cleanup
	} // End if

	if _, err := os.Stat(finalPath); err == nil { // Same filename now exists with different content
		filename = contentHash[:8] + "_" + filename                                          // Prefix with part of the hash to keep both files
		finalPath = filepath.Join(pdfDirectoryName, filename)                                // Rebuild the destination path
		slog.Debug("filename collision with different content, renaming", "file", finalPath) // Log the rename
	} // End if

	if err := os.Rename(temporaryPath, finalPath); err != nil { // Atomically move the temp file into place
		return fmt.Errorf("moving file into place: %w", err) // Report a move failure
	} // End if
	keepTemporaryFile = true                                                        // Prevent the deferred cleanup from deleting it
	registry.hashToFilename[contentHash] = filename                                 // Register the new hash
	slog.Info("saved PDF", "url", pdfURL, "file", finalPath, "sha256", contentHash) // Log the successful save
	return nil                                                                      // Success
} // End downloadPDF
