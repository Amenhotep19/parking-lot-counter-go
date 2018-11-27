package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gocv.io/x/gocv"
)

const (
	// name is a program name
	name = "parking-lot-counter"
	// topic is MQTT topic
	topic = "parking/counter"
)

var (
	// deviceID is camera device ID
	deviceID int
	// input is path to image or video file
	input string
	// model is path to .bin file of face detection model
	model string
	// modelConfig is path to .xml file of face detection model modelConfiguration
	modelConfig string
	// modelConfidence is confidence threshold for face detection model
	modelConfidence float64
	// backend is inference backend
	backend int
	// target is inference target
	target int
	// entrance defines axis for parking entrance and exit division mark
	entrance string
	// maxDist is max distance in pixels between two related centroids to be considered the same
	maxDist int
	// maxGone is max number of frames to track the centroid which doesnt change to be considered gone
	maxGone int
	// publish is a flag which instructs the program to publish data analytics
	publish bool
	// rate is number of seconds between analytics are collected and sent to a remote server
	rate int
	// delay is video playback delay
	delay float64
)

func init() {
	flag.IntVar(&deviceID, "device", -1, "Camera device ID")
	flag.StringVar(&input, "input", "", "Path to image or video file")
	flag.StringVar(&model, "model", "", "Path to .bin file of car detection model")
	flag.StringVar(&modelConfig, "model-config", "", "Path to .xml file of car model modelConfiguration")
	flag.Float64Var(&modelConfidence, "model-confidence", 0.5, "Confidence threshold for car detection")
	flag.IntVar(&backend, "backend", 0, "Inference backend. 0: Auto, 1: Halide language, 2: Intel DL Inference Engine")
	flag.IntVar(&target, "target", 0, "Target device. 0: CPU, 1: OpenCL, 2: OpenCL half precision, 3: VPU")

	flag.StringVar(&entrance, "entrance", "b", "Plane axis for parking entrance and exit division mark. b: Bottom frame, t: Top frame, l: Left frame, r: Right frame")
	flag.IntVar(&maxDist, "max-dist", 200, "Max distance in pixels between two centroids to be considered the same")
	flag.IntVar(&maxGone, "max-gone", 0, "Max number of frames to track the centroid which doesnt change to be considered gone")
	flag.BoolVar(&publish, "publish", false, "Publish data analytics to a remote server")
	flag.IntVar(&rate, "rate", 1, "Number of seconds between analytics are sent to a remote server")
	flag.Float64Var(&delay, "delay", 5.0, "Video playback delay")
}

// Perf stores inference engine performance info
type Perf struct {
	// CarNet stores face detector performance info
	CarNet float64
}

// String implements fmt.Stringer interface for Perf
func (p *Perf) String() string {
	return fmt.Sprintf("Car inference time: %.2f ms", p.CarNet)
}

// Centroid is car centroid
type Centroid struct {
	// ID is centroid ID
	ID string
	// Point is centeer point of the centroid
	Point image.Point
	// goneCount is number of frames centroid has been marked as gone
	goneCount int
}

// Result is monitoring computation result returned to main goroutine
type Result struct {
	// Centroids are current car centroids
	Centroids []*Centroid
	// CarsIn is a counter for cars entering the parking lot
	CarsIn int
	// CarsOut is a counter for cars leaving the parking lot
	CarsOut int
}

// String implements fmt.Stringer interface for Result
func (r *Result) String() string {
	return fmt.Sprintf("Cars In %d, Cars Out: %d", r.CarsIn, r.CarsOut)
}

// ToMQTTMessage turns result into MQTT message which can be published to MQTT broker
func (r *Result) ToMQTTMessage() string {
	return fmt.Sprintf("{\"TOTAL_IN\":%d, \"TOTAL_OUT\": %d}", r.CarsIn, r.CarsOut)
}

// getPerformanceInfo queries the Inference Engine performance info and returns it as string
func getPerformanceInfo(carNet *gocv.Net) *Perf {
	freq := gocv.GetTickFrequency() / 1000

	carPerf := carNet.GetPerfProfile() / freq

	return &Perf{
		CarNet: carPerf,
	}
}

// messageRunner reads data published to pubChan with rate frequency and sends them to remote analytics server
// doneChan is used to receive a signal from the main goroutine to notify the routine to stop and return
func messageRunner(doneChan <-chan struct{}, pubChan <-chan *Result, c *MQTTClient, topic string, rate int) error {
	ticker := time.NewTicker(time.Duration(rate) * time.Second)

	for {
		select {
		case <-ticker.C:
			result := <-pubChan
			_, err := c.Publish(topic, result.ToMQTTMessage())
			// TODO: decide whether to return with error and stop program;
			// For now we just signal there was an error and carry on
			if err != nil {
				fmt.Printf("Error publishing message to %s: %v", topic, err)
			}
		case <-pubChan:
			// we discard messages in between ticker times
		case <-doneChan:
			fmt.Printf("Stopping messageRunner: received stop sginal\n")
			return nil
		}
	}

	return nil
}

// detectCars detects cars in img and returns them as a slice of rectangles that encapsulates them
func detectCars(net *gocv.Net, img *gocv.Mat) []image.Rectangle {
	// convert img Mat to 672x384 blob that the face detector can analyze
	blob := gocv.BlobFromImage(*img, 1.0, image.Pt(672, 384), gocv.NewScalar(0, 0, 0, 0), false, false)
	defer blob.Close()

	// run a forward pass through the network
	net.SetInput(blob, "")
	results := net.Forward("")
	defer results.Close()

	// iterate through all detections and append results to cars buffer
	var cars []image.Rectangle
	for i := 0; i < results.Total(); i += 7 {
		confidence := results.GetFloatAt(0, i+2)
		if float64(confidence) > modelConfidence {
			left := int(results.GetFloatAt(0, i+3) * float32(img.Cols()))
			top := int(results.GetFloatAt(0, i+4) * float32(img.Rows()))
			right := int(results.GetFloatAt(0, i+5) * float32(img.Cols()))
			bottom := int(results.GetFloatAt(0, i+6) * float32(img.Rows()))
			cars = append(cars, image.Rect(left, top, right, bottom))
		}
	}

	return cars
}

// extractCenterPoints extracts centroid candidate center points from detected cars and returns them
func extractCenterPoints(cars []image.Rectangle, img *gocv.Mat) []image.Point {
	var centerPoints []image.Point
	// detected car size in pixels
	var width, height int
	// center point coordinates
	var X, Y int
	// width and height pixel clips
	wClip, hClip := 200, 350

	// make sure the car rect is completely inside the image frame
	for i := range cars {
		if !cars[i].In(image.Rect(0, 0, img.Cols(), img.Rows())) {
			continue
		}

		// detected car dimensions
		width = cars[i].Size().X
		height = cars[i].Size().Y
		// if detected car is too small, skip it
		if width < 70 || height < 70 {
			continue
		}

		// Sometimes detected car rectangle stretches way over the actual car dimensions
		// so we clip the sizes of the rectangle to avoid skewing the centroid positions
		if width > wClip {
			if cars[i].Min.X+wClip < img.Cols() {
				width = wClip
			}
		} else if cars[i].Min.X+width > img.Cols() {
			width = img.Cols() - cars[i].Min.X
		}

		if height > hClip {
			if cars[i].Min.Y+hClip < img.Cols() {
				height = hClip
			}
		} else if cars[i].Min.Y+height > img.Rows() {
			height = img.Rows() - cars[i].Min.Y
		}

		// center point coordinates
		X = cars[i].Min.X + width/2
		Y = cars[i].Min.Y + height/2

		centerPoints = append(centerPoints, image.Point{X: X, Y: Y})
	}

	return centerPoints
}

// frameRunner reads image frames from framesChan and performs face and sentiment detections on them
// doneChan is used to receive a signal from the main goroutine to notify frameRunner to stop and return
func frameRunner(framesChan <-chan *frame, doneChan <-chan struct{}, resultsChan chan<- *Result,
	perfChan chan<- *Perf, pubChan chan<- *Result, carNet *gocv.Net) error {

	for {
		select {
		case <-doneChan:
			fmt.Printf("Stopping frameRunner: received stop sginal\n")
			return nil
		case frame := <-framesChan:
			// let's make a copy of the original
			img := gocv.NewMat()
			frame.img.CopyTo(&img)

			// detect cars and return them
			cars := detectCars(carNet, &img)
			// extract car center points: not all car detections are valid
			centerPoints := extractCenterPoints(cars, &img)

			// TODO: implement the centroid updates functions
			fmt.Println(centerPoints)

			// detection result
			result := &Result{}

			// send data down the channels
			perfChan <- getPerformanceInfo(carNet)
			resultsChan <- result
			if pubChan != nil {
				pubChan <- result
			}

			// close image matrices
			img.Close()
		}
	}

	return nil
}

func parseCliFlags() error {
	// parse cli flags
	flag.Parse()

	// path to face detection model can't be empty
	if model == "" {
		return fmt.Errorf("Invalid path to .bin file of face detection model: %s", model)
	}
	// path to face detection model modelConfig can't be empty
	if modelConfig == "" {
		return fmt.Errorf("Invalid path to .xml file of face model modelConfiguration: %s", modelConfig)
	}

	return nil
}

// NewInferModel reads DNN model and it modelConfiguration, sets its preferable target and backend and returns it.
// It returns error if either the model files failed to be read or setting the target fails
func NewInferModel(model, modelConfig string, backend, target int) (*gocv.Net, error) {
	// read in car model and set the target
	m := gocv.ReadNet(model, modelConfig)

	if err := m.SetPreferableBackend(gocv.NetBackendType(backend)); err != nil {
		return nil, err
	}

	if err := m.SetPreferableTarget(gocv.NetTargetType(target)); err != nil {
		return nil, err
	}

	return &m, nil
}

// NewCapture creates new video capture from input or camera backend if input is empty and returns it.
// If input is not empty, NewCapture adjusts delay parameter so video playback matches FPS in the video file.
// It fails with error if it either can't open the input video file or the video device
func NewCapture(input string, deviceID int, delay *float64) (*gocv.VideoCapture, error) {
	if input != "" {
		// open video file
		vc, err := gocv.VideoCaptureFile(input)
		if err != nil {
			return nil, err
		}

		fps := vc.Get(gocv.VideoCaptureFPS)
		*delay = 1000 / fps

		return vc, nil
	}

	// open camera device
	vc, err := gocv.VideoCaptureDevice(deviceID)
	if err != nil {
		return nil, err
	}

	return vc, nil
}

// NewMQTTPublisher creates new MQTT client which collects analytics data and publishes them to remote MQTT server.
// It attempts to make a connection to the remote server and if successful it return the client handler
// It returns error if either the connection to the remote server failed or if the client modelConfig is invalid.
func NewMQTTPublisher() (*MQTTClient, error) {
	// create MQTT client and connect to MQTT server
	opts, err := MQTTClientOptions()
	if err != nil {
		return nil, err
	}

	// create MQTT client ad connect to remote server
	c, err := MQTTConnect(opts)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// frame ise used to send video frames and program modelConfiguration to upstream goroutines
type frame struct {
	// img is image frame
	img *gocv.Mat
}

func main() {
	// parse cli flags
	if err := parseCliFlags(); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing command line parameters: %v\n", err)
		os.Exit(1)
	}

	// read in car detection model and set its inference backend and target
	carNet, err := NewInferModel(model, modelConfig, backend, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating car detection model: %v\n", err)
		os.Exit(1)
	}

	// create new video capture
	vc, err := NewCapture(input, deviceID, &delay)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating new video capture: %v\n", err)
		os.Exit(1)
	}
	defer vc.Close()

	// frames channel provides the source of images to process
	framesChan := make(chan *frame, 1)
	// errChan is a channel used to capture program errors
	errChan := make(chan error, 2)
	// doneChan is used to signal goroutines they need to stop
	doneChan := make(chan struct{})
	// resultsChan is used for detection distribution
	resultsChan := make(chan *Result, 1)
	// perfChan is used for collecting performance stats
	perfChan := make(chan *Perf, 1)
	// sigChan is used as a handler to stop all the goroutines
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, os.Kill, syscall.SIGTERM)
	// pubChan is used for publishing data analytics stats
	var pubChan chan *Result
	// waitgroup to synchronise all goroutines
	var wg sync.WaitGroup

	if publish {
		p, err := NewMQTTPublisher()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create MQTT publisher: %v\n", err)
			os.Exit(1)
		}
		pubChan = make(chan *Result, 1)
		// start MQTT worker goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			errChan <- messageRunner(doneChan, pubChan, p, topic, rate)
		}()
		defer p.Disconnect(100)
	}

	// start frameRunner goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		errChan <- frameRunner(framesChan, doneChan, resultsChan, perfChan, pubChan, carNet)
	}()

	// open display window
	window := gocv.NewWindow(name)
	window.SetWindowProperty(gocv.WindowPropertyFullscreen, gocv.WindowAutosize)
	defer window.Close()

	// prepare input image matrix
	img := gocv.NewMat()
	defer img.Close()

	// initialize the result pointers
	result := new(Result)
	perf := new(Perf)

monitor:
	for {
		if ok := vc.Read(&img); !ok {
			fmt.Printf("Cannot read image source %v\n", deviceID)
			break
		}
		if img.Empty() {
			continue
		}

		framesChan <- &frame{img: &img}

		select {
		case sig := <-sigChan:
			fmt.Printf("Shutting down. Got signal: %s\n", sig)
			break monitor
		case err = <-errChan:
			fmt.Printf("Shutting down. Encountered error: %s\n", err)
			break monitor
		case result = <-resultsChan:
			perf = <-perfChan
		default:
			// do nothing; just display latest results
		}
		// inference performance and print it
		gocv.PutText(&img, fmt.Sprintf("%s", perf), image.Point{0, 25},
			gocv.FontHersheySimplex, 0.5, color.RGBA{255, 255, 255, 0}, 2)
		// inference results label
		gocv.PutText(&img, fmt.Sprintf("%s", result), image.Point{0, 45},
			gocv.FontHersheySimplex, 0.5, color.RGBA{255, 255, 255, 0}, 2)
		// TODO: Draw car centroids
		// show the image in the window, and wait 1 millisecond
		window.IMShow(img)

		// exit when ESC key is pressed
		if window.WaitKey(int(delay)) >= 27 {
			break monitor
		}
	}
	// signal all goroutines to finish
	close(doneChan)
	// wait for all goroutines to finish
	wg.Wait()
}
