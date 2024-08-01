package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/go-vgo/robotgo"
	"github.com/gordonklaus/portaudio"
	hook "github.com/robotn/gohook"
)

const (
	// audio
	sampleRate = 44100
	channels   = 1

	// open ai api
	openAIURL   = "https://api.openai.com/v1/audio/transcriptions"
	openAIModel = "whisper-1"

	// trigger
	globeKeyCode    = 179
	doublePressTime = 500 * time.Millisecond
)

var (
	openAIKey string
	dictating bool
)

func main() {
	// Read OpenAI API key from environment variable
	if envKey := os.Getenv("OPENAI_API_KEY"); envKey != "" {
		openAIKey = envKey
	} else {
		fmt.Println("Error: OPENAI_API_KEY environment variable not set.")
		os.Exit(1)
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := portaudio.Initialize(); err != nil {
		return fmt.Errorf("initializing portaudio: %w", err)
	}
	defer portaudio.Terminate()

	// We are using a context to handle the interrupt signal sent by kill command
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// The program can be quit by two different methods,
	// First one being the user pressing `CTRL` + `C` which in turn should send a `SIGINT` signal to the program but for me iTerm doesn't so we are handling that manually by tracking keyboard presses since we are already doing that for triggering the recording
	// Second way is using a `kill` command which in turn sends the `SIGINT`` signal to the program which the program handles correctly.

	go func() {
		<-ctx.Done()
		fmt.Println("Received interrupt signal.")
	}()

	// Pass the cancel function as well because we are tracking the control plus C press manually using raw codes hence we need to invoke the cancel function
	listenForKeyboardEvents(ctx, cancel)

	fmt.Println("Shutting down now...")
	return nil
}

func listenForKeyboardEvents(ctx context.Context, cancel context.CancelFunc) {
	fmt.Println("Starting keyboard listener. Press Ctrl+C to exit.")

	evChan := hook.Start()
	defer hook.End()

	var lastGlobePressTime time.Time
	ctrlPressed := false

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Context cancelled, stopping keyboard listener")
			return
		case ev := <-evChan:
			if ev.Kind == hook.KeyHold || ev.Kind == hook.KeyDown {
				if ev.Rawcode == 59 { // Ctrl press
					ctrlPressed = true
				} else if ev.Rawcode == 8 && ctrlPressed { // Ctrl + C
					fmt.Println("User pressed Ctrl+C")
					cancel()
					return
				} else {
					ctrlPressed = false
					handleKeyEvent(ctx, ev, &lastGlobePressTime)
				}
			} else if ev.Kind == hook.KeyUp { // don't release Ctrl if you want to quit program
				if ev.Rawcode == 59 {
					ctrlPressed = false
				}
			}
		}
	}
}

func handleKeyEvent(ctx context.Context, ev hook.Event, lastGlobePressTime *time.Time) {
	if ev.Rawcode != globeKeyCode {
		return
	}

	now := time.Now()
	if now.Sub(*lastGlobePressTime) < doublePressTime {
		handleDoublePress(ctx)
	} else {
		handleSinglePress()
	}
	*lastGlobePressTime = now
}

func handleDoublePress(ctx context.Context) {
	if !dictating {
		fmt.Println("Double press detected, starting transcription")
		dictating = true
		go startTranscription(ctx)
	}
}

func handleSinglePress() {
	if dictating {
		fmt.Println("Single press detected, stopping transcription")
		dictating = false
	}
}

func startTranscription(ctx context.Context) {
	audioFilePath, err := recordAudio(ctx)
	if err != nil {
		fmt.Printf("Error saving audio file: %v\n", err)
		return
	}

	transcription, err := transcribeAudio(audioFilePath)
	if err != nil {
		fmt.Printf("Error transcribing: %v\n", err)
		return
	}

	fmt.Printf("You said: %s\n", transcription)
	robotgo.TypeStr(transcription)
}

func transcribeAudio(audioFilePath string) (string, error) {
	file, err := os.Open(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("opening audio file: %w", err)
	}
	defer file.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", audioFilePath)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("copying file to form: %w", err)
	}

	if err := writer.WriteField("model", openAIModel); err != nil {
		return "", fmt.Errorf("writing model field: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("closing multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", openAIURL, body)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+openAIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Text string `json:"text"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}

	if err := os.Remove(audioFilePath); err != nil {
		fmt.Printf("Warning: failed to remove temporary audio file: %v\n", err)
	}

	return result.Text, nil
}

func recordAudio(ctx context.Context) (string, error) {
	buffer := make([]float32, 1024)
	stream, err := portaudio.OpenDefaultStream(channels, 0, float64(sampleRate), len(buffer), buffer)
	if err != nil {
		return "", fmt.Errorf("opening audio stream: %w", err)
	}
	defer stream.Close()

	var allSamples []float32

	if err := stream.Start(); err != nil {
		return "", fmt.Errorf("starting audio stream: %w", err)
	}

	fmt.Println("Recording... Press the dictation key again to stop.")

	recordingDone := make(chan struct{})
	go func() {
		for dictating {
			select {
			case <-ctx.Done():
				fmt.Println("Context cancelled, stopping recording")
				return
			default:
				fmt.Print(".")
				if err := stream.Read(); err != nil {
					fmt.Printf("Error reading from stream: %v\n", err)
					return
				}
				allSamples = append(allSamples, buffer...)
			}
		}
		fmt.Println("stopping recording")
		close(recordingDone)
	}()

	// Wait for either context cancellation or recording to finish
	select {
	case <-ctx.Done():
		fmt.Println("Context cancelled, recording stopped")
	case <-recordingDone:
		fmt.Println("Recording finished")
	}

	dictating = false // Ensure dictating is set to false

	if err := stream.Stop(); err != nil {
		return "", fmt.Errorf("stopping audio stream: %w", err)
	}

	return saveAudioToFile(allSamples)
}

func saveAudioToFile(samples []float32) (string, error) {
	filename := fmt.Sprintf("recorded_audio_%s.wav", time.Now().Format("20060102_150405"))
	fullPath, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("getting absolute path: %w", err)
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return "", fmt.Errorf("creating audio file: %w", err)
	}
	defer file.Close()

	intBuffer := make([]int, len(samples))
	for i, sample := range samples {
		intBuffer[i] = int(sample * 32767)
	}

	wavEncoder := wav.NewEncoder(file, sampleRate, 16, channels, 1)
	defer wavEncoder.Close()

	audioIntBuffer := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: channels,
			SampleRate:  sampleRate,
		},
		Data:           intBuffer,
		SourceBitDepth: 16,
	}

	if err := wavEncoder.Write(audioIntBuffer); err != nil {
		return "", fmt.Errorf("encoding WAV: %w", err)
	}

	return fullPath, nil
}
