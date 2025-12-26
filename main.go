package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gen2brain/beeep"
	"github.com/kbinani/screenshot"
)

// Config lets you tune detection.
type Config struct {
	PollInterval            time.Duration
	RedMinR                 uint8
	RedMaxG                 uint8
	RedMaxB                 uint8
	MinRedPixelsPerRow      int
	MaxDistanceBubbleToLine int
	BubbleBrightThreshold   int
	BubbleMinBrightPixels   int
	ROIMarginPercent        float64
}

func main() {
	cfg := Config{
		PollInterval:            10 * time.Second,
		RedMinR:                 180,
		RedMaxG:                 120, // allow orange/yellow, not just pure red
		RedMaxB:                 120,
		MinRedPixelsPerRow:      500,  // tune by screen size
		MaxDistanceBubbleToLine: 10,   // pixels above/below line
		BubbleBrightThreshold:   600,  // r+g+b >= this
		BubbleMinBrightPixels:   150,  // how many “bright” pixels = bubble
		ROIMarginPercent:        0.10, // ignore outer 10% around screen
	}

	log.Println("Bookmap watcher (macOS) started...")

	for {
		if err := checkOnce(cfg); err != nil {
			log.Println("error:", err)
		}
		time.Sleep(cfg.PollInterval)
	}
}

func checkOnce(cfg Config) error {
	img, err := captureMainDisplay()

	if err != nil {
		return err
	}

	roi := centralROI(img.Bounds(), cfg.ROIMarginPercent)

	// TODO: if you want a real ML step to check “is Bookmap open?”,
	// put it here. For now we assume Bookmap is visible in ROI.

	lineY, ok := findRedLine(img, roi, cfg)

	log.Println("img ", img)

	// Save image to file
	imgFilePath := "current_screenshot.png"
	if err := saveImageToFile(img, imgFilePath); err != nil {
		log.Println("error saving image:", err)
		return err
	}

	// Pass image to AI model to find maximum order red line
	stockPrice, err := getStockPriceFromAI(imgFilePath)
	if err != nil {
		log.Println("error getting stock price from AI:", err)
		return err
	}

	log.Printf("Stock price detected: $%.2f\n", stockPrice)

	if !ok {
		return nil // no red line this frame
	}

	if bubbleAtLine(img, roi, lineY, cfg) {
		go triggerAlert(lineY)
	}
	return nil
}

// captureMainDisplay grabs display 0 on macOS.
func captureMainDisplay() (image.Image, error) {
	if screenshot.NumActiveDisplays() == 0 {
		return nil, errString("no active displays found")
	}
	return screenshot.CaptureDisplay(0)
}

// centralROI cuts off a margin around the screen (menu bar / dock / junk).
func centralROI(bounds image.Rectangle, marginPct float64) image.Rectangle {
	w := bounds.Dx()
	h := bounds.Dy()

	marginX := int(float64(w) * marginPct)
	marginY := int(float64(h) * marginPct)

	return image.Rect(
		bounds.Min.X+marginX,
		bounds.Min.Y+marginY,
		bounds.Max.X-marginX,
		bounds.Max.Y-marginY,
	)
}

// findRedLine searches each row in ROI for one with a lot of red/orange pixels.
func findRedLine(img image.Image, roi image.Rectangle, cfg Config) (int, bool) {
	maxCount := 0
	bestY := -1

	for y := roi.Min.Y; y < roi.Max.Y; y++ {
		count := 0
		for x := roi.Min.X; x < roi.Max.X; x++ {
			r16, g16, b16, _ := img.At(x, y).RGBA()
			r := uint8(r16 >> 8)
			g := uint8(g16 >> 8)
			b := uint8(b16 >> 8)

			if isLineRed(r, g, b, cfg) {
				count++
			}
		}
		if count > maxCount && count >= cfg.MinRedPixelsPerRow {
			maxCount = count
			bestY = y
		}
	}

	if bestY >= 0 {
		log.Printf("Red line near Y=%d (%d red pixels)\n", bestY, maxCount)
		return bestY, true
	}
	return 0, false
}

func isLineRed(r, g, b uint8, cfg Config) bool {
	// strong R, limited G/B → red/orange horizontal heat lines
	return r >= cfg.RedMinR && g <= cfg.RedMaxG && b <= cfg.RedMaxB
}

// bubbleAtLine looks for a bright “bubble” near the right edge at the same Y.
func bubbleAtLine(img image.Image, roi image.Rectangle, lineY int, cfg Config) bool {
	log.Println("bubbleAtLine.")
	width := roi.Dx()
	// search in right 20% of ROI
	xStart := roi.Min.X + int(float64(width)*0.8)
	xEnd := roi.Max.X

	yMin := lineY - cfg.MaxDistanceBubbleToLine
	yMax := lineY + cfg.MaxDistanceBubbleToLine
	if yMin < roi.Min.Y {
		yMin = roi.Min.Y
	}
	if yMax > roi.Max.Y {
		yMax = roi.Max.Y
	}

	brightCount := 0
	for y := yMin; y < yMax; y++ {
		for x := xStart; x < xEnd; x++ {
			r16, g16, b16, _ := img.At(x, y).RGBA()
			r := uint8(r16 >> 8)
			g := uint8(g16 >> 8)
			b := uint8(b16 >> 8)

			if isBubbleBright(r, g, b, cfg) {
				brightCount++
			}
		}
	}

	if brightCount >= cfg.BubbleMinBrightPixels {
		log.Printf("Bubble detected near line at Y=%d (%d bright pixels)\n", lineY, brightCount)
		return true
	}
	return false
}

func isBubbleBright(r, g, b uint8, cfg Config) bool {
	sum := int(r) + int(g) + int(b)
	return sum >= cfg.BubbleBrightThreshold
}

// triggerAlert fires a macOS notification + sound.
func triggerAlert(lineY int) {
	title := "Bookmap alert"
	msg := "Price bubble reached red line (Y=" + itoa(lineY) + ")"

	if err := beeep.Notify(title, msg, ""); err != nil {
		log.Println("notify error:", err)
	}
	if err := beeep.Beep(880, 500); err != nil {
		log.Println("beep error:", err)
	}
	log.Println("ALERT:", msg)
}

// tiny helpers to avoid extra imports
type errString string

func (e errString) Error() string { return string(e) }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = '0' + byte(v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// saveImageToFile saves an image.Image to a PNG file.
func saveImageToFile(img image.Image, filePath string) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	if err := png.Encode(file, img); err != nil {
		return fmt.Errorf("failed to encode image: %w", err)
	}

	log.Printf("Image saved to %s\n", filePath)
	return nil
}

// AIResponse represents the expected response from the AI modal.
type AIResponse struct {
	StockPrice float64 `json:"stockPrice"`
}

// getStockPriceFromAI sends the image file to an AI model and expects a response with stockPrice.
// You'll need to replace "YOUR_AI_API_ENDPOINT" with your actual AI service endpoint.
func getStockPriceFromAI(imgFilePath string) (float64, error) {
	// Open the image file
	file, err := os.Open(imgFilePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open image file: %w", err)
	}
	defer file.Close()

	// Read the file content
	fileContent, err := io.ReadAll(file)
	if err != nil {
		return 0, fmt.Errorf("failed to read image file: %w", err)
	}

	// Create a multipart form request to send the image to the AI API
	// Replace "YOUR_AI_API_ENDPOINT" with your actual AI service endpoint
	apiEndpoint := "http://localhost:8000/api/detect-stock-price" // TODO: Configure your AI API endpoint

	// body := &bytes.Buffer{}
	// For now, sending as raw binary. Adjust based on your API requirements.
	// If your API expects form data, you may need to use multipart/form-data

	req, err := http.NewRequest("POST", apiEndpoint, bytes.NewReader(fileContent))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "image/png")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send request to AI service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("AI service returned status %d", resp.StatusCode)
	}

	// Parse the response
	var aiResponse AIResponse
	if err := json.NewDecoder(resp.Body).Decode(&aiResponse); err != nil {
		return 0, fmt.Errorf("failed to decode AI response: %w", err)
	}

	return aiResponse.StockPrice, nil
}
