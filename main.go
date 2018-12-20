/*
* Copyright (c) 2018 Intel Corporation.
*
* Permission is hereby granted, free of charge, to any person obtaining
* a copy of this software and associated documentation files (the
* "Software"), to deal in the Software without restriction, including
* without limitation the rights to use, copy, modify, merge, publish,
* distribute, sublicense, and/or sell copies of the Software, and to
* permit persons to whom the Software is furnished to do so, subject to
* the following conditions:
*
* The above copyright notice and this permission notice shall be
* included in all copies or substantial portions of the Software.
*
* THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
* EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
* MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
* NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
* LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
* OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
* WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
 */

package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
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
	flag.IntVar(&maxDist, "max-dist", 300, "Max distance in pixels between two centroids to be considered the same")
	flag.IntVar(&maxGone, "max-gone", 30, "Max number of frames to track the centroid which doesnt change to be considered gone")
	flag.BoolVar(&publish, "publish", false, "Publish data analytics to a remote server")
	flag.IntVar(&rate, "rate", 1, "Number of seconds between analytics are sent to a remote server")
	flag.Float64Var(&delay, "delay", 5.0, "Video playback delay")
}

// Perf stores inference engine performance info
type Perf struct {
	// Net stores face detector performance info
	Net float64
}

// String implements fmt.Stringer interface for Perf
func (p *Perf) String() string {
	return fmt.Sprintf("Inference time: %.2f ms", p.Net)
}

// Direction is car direction
type Direction int

const (
	// UP means car is moving up
	UP Direction = iota + 1
	// DOWN means car is moving down
	DOWN
	// LEFT means car is moving left
	LEFT
	// RIGHT means car is moving right
	RIGHT
	// STILL means car is not moving
	STILL
)

// String implements fmt.Stringer for Direction
func (d Direction) String() string {
	switch d {
	case UP:
		return "UP"
	case DOWN:
		return "DOWN"
	case LEFT:
		return "LEFT"
	case RIGHT:
		return "RIGHT"
	case STILL:
		return "STILL"
	default:
		return "UNKNOWN"

	}
}

// Centroid is car centroid
type Centroid struct {
	// ID is centroid ID
	ID uuid.UUID
	// Point is centeer point of the centroid
	Point image.Point
	// goneCount is number of frames centroid has been marked as gone
	goneCount int
}

// String implements fmt.Stringer for Car
func (c Centroid) String() string {
	return fmt.Sprintf("%v", c.Point)
}

// Car is a tracked car
type Car struct {
	// ID is car ID
	ID uuid.UUID
	// Traject is car trajectory
	Traject []image.Point
	// Dir is car direction
	Dir Direction
	// counted is used to flag car as counted
	counted bool
	// gone is used to mark the car as gone
	gone bool
}

// String implements fmt.Stringer for Car
func (c Car) String() string {
	//return fmt.Sprintf("ID: %s, Trajectory: %v, Direction: %s", c.ID, c.Traject, c.Dir)
	return fmt.Sprintf("ID: %s, Traject: %v, Dir: %s", c.ID, c.Traject, c.Dir)
}

// MeanMovement calculates movement of car along movement axis realtive to the entrance position.
// Car movement is calculated as a mean value of all the previous poisitions of the car centroids.
func (c Car) MeanMovement() float64 {
	meanMov := 0.0

	// if no trajectory yet, return 0.0
	if len(c.Traject) == 0 {
		return meanMov
	}

	for i := range c.Traject {
		// if entrance is vertical i.e. car moves LEFT<->RIGHT only consider trajectory along X axis
		if strings.EqualFold(entrance, "l") || strings.EqualFold(entrance, "r") {
			meanMov = meanMov + float64(c.Traject[i].X)
		}
		// if entrance is horizontal i.e. car moves TOP<->BOTTOM only consider trajectory along Y axis
		if strings.EqualFold(entrance, "b") || strings.EqualFold(entrance, "t") {
			meanMov = meanMov + float64(c.Traject[i].Y)
		}
	}

	return meanMov / float64(len(c.Traject))
}

// Direction calculates the direction of the car movement along particular movement axis based on entrance
// as a difference between current car position p and its mean movement and returns it
func (c Car) Direction(p image.Point) Direction {
	meanMov := c.MeanMovement()

	// if entrance is vertical i.e. car moves LEFT<->RIGHT only consider trajectory along X axis
	if strings.EqualFold(entrance, "l") || strings.EqualFold(entrance, "r") {
		if p.X-int(meanMov) > 0 {
			return RIGHT
		}

		if p.X-int(meanMov) < 0 {
			return LEFT
		}
	}

	// if entrance is horizontal i.e. car moves TOP<->BOTTOM only consider trajectory along Y axis
	if strings.EqualFold(entrance, "b") || strings.EqualFold(entrance, "t") {
		if p.Y-int(meanMov) > 0 {
			return DOWN
		}

		if p.Y-int(meanMov) < 0 {
			return UP
		}
	}

	return STILL
}

// CarMap is a map of tracked cars
type CarMap map[uuid.UUID]*Car

// Add adds new car with centroid c to the map of tracked cars.
// It retruns bool to report if the addition was successful
func (cm CarMap) Add(c *Centroid) bool {
	traject := make([]image.Point, 0)
	traject = append(traject, c.Point)

	car := &Car{
		ID:      c.ID,
		Traject: traject,
		Dir:     STILL,
		counted: false,
		gone:    false,
	}

	cm[c.ID] = car

	return false
}

// Remove removes centroid with id
func (cm CarMap) Remove(id uuid.UUID) {
	delete(cm, id)
}

// Update updates tracked car map with centroids.
// It updates tracking info of the tracked centroids and starts tracking new centroids.
func (cm CarMap) Update(centroids CentroidMap) {
	// mark the cars which disappeared from centroids as gone
	for id, _ := range cm {
		if _, present := centroids[id]; !present {
			if cm[id].gone && cm[id].Dir == STILL {
				cm.Remove(id)
			} else {
				cm[id].gone = true
			}
		}
	}

	// start tracking new centroids i.e. cars
	for id, _ := range centroids {
		if _, tracked := cm[id]; !tracked {
			cm.Add(centroids[id])
		} else {
			cm[id].Traject = append(cm[id].Traject, centroids[id].Point)
			cm[id].Dir = cm[id].Direction(centroids[id].Point)
		}
	}
}

// ParkingLot is a parking lot
type ParkingLot struct {
	// TotalIn is a counter that counts cars entering the parking lot
	TotalIn int
	// TotalOut is a counter that counts cars leaving the parking lot
	TotalOut int
}

// Update updates parking lot counters using the tracked cars.
func (p *ParkingLot) Update(cars CarMap) {
	// iterate through all cars and update global counters
	for id, _ := range cars {
		if !cars[id].counted {
			if !cars[id].gone {
				switch entrance {
				case "t":
					if cars[id].Dir == DOWN {
						p.TotalIn++
						cars[id].counted = true
					}
				case "l":
					if cars[id].Dir == RIGHT {
						p.TotalIn++
						cars[id].counted = true
					}
				case "b":
					if cars[id].Dir == UP {
						p.TotalIn++
						cars[id].counted = true
					}
				case "r":
					if cars[id].Dir == LEFT {
						p.TotalIn++
						cars[id].counted = true
					}
				}
			} else {
				switch entrance {
				case "t":
					if cars[id].Dir == UP {
						p.TotalOut++
						cars.Remove(id)
					}
				case "l":
					if cars[id].Dir == LEFT {
						p.TotalOut++
						cars.Remove(id)
					}
				case "b":
					if cars[id].Dir == DOWN {
						p.TotalOut++
						cars.Remove(id)
					}
				case "r":
					if cars[id].Dir == RIGHT {
						p.TotalOut++
						cars.Remove(id)
					}
				}
			}
		} else {
			if cars[id].gone {
				cars.Remove(id)
			}
		}
	}
}

// CentroidMap is a map of car centroids.
type CentroidMap map[uuid.UUID]*Centroid

// Add adds new centroid p to centroid map.
// It retruns bool to signal if the addition was successful or not.
func (cm CentroidMap) Add(p image.Point) bool {
	ID := uuid.New()

	c := &Centroid{
		ID:        ID,
		Point:     p,
		goneCount: 0,
	}

	cm[ID] = c

	return true
}

// Remove removes centroid with id from centroid map.
func (cm CentroidMap) Remove(id uuid.UUID) {
	delete(cm, id)
}

// Update updates centroid map based on centerpoints
func (cm CentroidMap) Update(points []image.Point) {
	// if no points are passed in, increment gone count of all existing centroids and
	// stop tracking the centroids which exceeded maxGone threshold
	if len(points) == 0 {
		for id, _ := range cm {
			cm[id].goneCount++
			if cm[id].goneCount > maxGone {
				cm.Remove(id)
			}
		}

		return
	}

	// mappedPoints keeps track of the points tha have been mapped to existing centroids
	mappedPoints := map[int]image.Point{}
	// updatedCentroids keeps track of the centroids that have been updated by points
	updatedCentroids := map[uuid.UUID]*Centroid{}

	// If no centroids are tracked yet, start tracking all new points
	// Otherwise update existing centroids with new points locations
	if len(cm) == 0 {
		for i := range points {
			cm.Add(points[i])
		}
	} else {
		for i := range points {
			id, dist := cm.ClosestDist(points[i])
			// if the distance from the point to the closest centroid is too large,
			// don't associate them together; also dont associate already associated points
			_, alreadyMapped := mappedPoints[i]
			if (dist > float64(maxDist)) || alreadyMapped {
				continue
			}
			// update position of the closest centroid and reset its goneCount
			cm[id].Point = points[i]
			cm[id].goneCount = 0
			// keep track of already mapped points and updated centroids
			mappedPoints[i] = points[i]
			updatedCentroids[id] = cm[id]
		}

		// iterate through already tracked centroids and increment their goneCount if they werent updated
		// if the centroid was NOT updated and it exceeds maxGone threshold, stop tracking it
		for id, _ := range cm {
			if _, ok := updatedCentroids[id]; !ok {
				cm[id].goneCount++
				if cm[id].goneCount > maxGone {
					cm.Remove(id)
				}
			}
		}

		// iterate through center points and start tracking the points that are NOT yet mapped to
		// any of the already tracked centroids i.e. add them in
		for i := range points {
			if _, ok := mappedPoints[i]; !ok {
				cm.Add(points[i])
			}
		}
	}

	return
}

// ClosestDist finds the closest centroid to p and returns both its ID and distance from p.
// ClosestDist uses euclidean distance as a measure of closeness.
func (cm CentroidMap) ClosestDist(p image.Point) (uuid.UUID, float64) {
	var minID uuid.UUID
	minDist := math.MaxFloat64

	for id, _ := range cm {
		// If entrance is vertical: the movement is LEFT<->RIGHT, only consider centroids with
		// some small Y coordinate fluctuation as Y coordinate should not be changing much
		if strings.EqualFold(entrance, "l") || strings.EqualFold(entrance, "r") {
			if (cm[id].Point.Y < (p.Y - 70)) || (cm[id].Point.Y > (p.Y + 70)) {
				continue
			}
		}
		// If entrance is horizontal: the movement is TOP<->BOTTOM, only consider centroids with
		// some small X coordinate fluctuation as X coordinate should not be changing much
		if strings.EqualFold(entrance, "b") || strings.EqualFold(entrance, "t") {
			if (cm[id].Point.X < (p.X - 50)) || (cm[id].Point.X > (p.X + 50)) {
				continue
			}
		}

		dx := float64(cm[id].Point.X - p.X)
		dy := float64(cm[id].Point.Y - p.Y)
		dist := math.Sqrt(dx*dx + dy*dy)

		if dist < minDist {
			minDist = dist
			minID = id
		}
	}

	return minID, minDist
}

// Result is monitoring computation result returned to main goroutine
type Result struct {
	// Perf is inference engine performance
	Perf *Perf
	// Centroids is a map of car centroids
	Centroids CentroidMap
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

// getPerformanceInfo queries the Inference Engine performance info and returns it
func getPerformanceInfo(net *gocv.Net) *Perf {
	freq := gocv.GetTickFrequency() / 1000

	perf := net.GetPerfProfile() / freq

	return &Perf{
		Net: perf,
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
func extractCenterPoints(rects []image.Rectangle, img *gocv.Mat) []image.Point {
	var centerPoints []image.Point
	// detected car size in pixels
	var width, height int
	// center point coordinates
	var X, Y int
	// width and height pixel clips
	wClip, hClip := 200, 350

	// make sure the car rect is completely inside the image frame
	for i := range rects {
		if !rects[i].In(image.Rect(0, 0, img.Cols(), img.Rows())) {
			continue
		}

		// detected car rectangle dimensions
		width = rects[i].Size().X
		height = rects[i].Size().Y
		// if detected car rectangle is too small, skip it
		if width < 80 || height < 50 {
			continue
		}

		// Sometimes detected car rectangle stretches way over the actual car dimensions
		// so we clip the sizes of the rectangle to avoid skewing the centroid positions
		// If the clipped size stretches over image frame we clip them with frame size.
		if width > wClip {
			if (rects[i].Min.X + wClip) < img.Cols() {
				width = wClip
			}
		} else if (rects[i].Min.X + width) > img.Cols() {
			width = img.Cols() - rects[i].Min.X
		}

		if height > hClip {
			if (rects[i].Min.Y + hClip) < img.Rows() {
				height = hClip
			}
		} else if (rects[i].Min.Y + height) > img.Rows() {
			height = img.Rows() - rects[i].Min.Y
		}

		// center point coordinates
		X = rects[i].Min.X + width/2
		Y = rects[i].Min.Y + height/2

		centerPoints = append(centerPoints, image.Point{X: X, Y: Y})
	}

	return centerPoints
}

// frameRunner reads image frames from framesChan and performs face and sentiment detections on them
// doneChan is used to receive a signal from the main goroutine to notify frameRunner to stop and return
func frameRunner(framesChan <-chan *frame, doneChan <-chan struct{}, resultsChan chan<- *Result,
	pubChan chan<- *Result, carNet *gocv.Net) error {

	// frame is image frame
	frame := new(frame)
	// result stores results to be sent down to main goroutine
	result := new(Result)
	// perf is inference engine performance
	perf := new(Perf)
	// centroid keeps the list of all tracked centroids
	centroids := make(CentroidMap)
	// cars keeps the list of all tracked cars
	cars := make(CarMap)
	// parkingLot is the parking lot we are monitoring
	parkingLot := new(ParkingLot)

	for {
		select {
		case <-doneChan:
			fmt.Printf("Stopping frameRunner: received stop sginal\n")
			// close results channel
			close(resultsChan)
			// close publish channel
			if pubChan != nil {
				close(pubChan)
			}
			return nil
		case frame = <-framesChan:
			if frame == nil {
				continue
			}
			// let's make a copy of the original
			img := gocv.NewMat()
			frame.img.CopyTo(&img)

			// detect cars in the current frame
			carRects := detectCars(carNet, &img)

			// extract car center points: not all car detections are valid cars
			centerPoints := extractCenterPoints(carRects, &img)

			// update tracked centroids with the points detected in the frame
			centroids.Update(centerPoints)

			// update tracked cars based on centroids
			cars.Update(centroids)

			// update parking lot counters
			parkingLot.Update(cars)

			perf = getPerformanceInfo(carNet)
			// detection result
			result.Perf = perf
			result.Centroids = centroids
			result.CarsIn = parkingLot.TotalIn
			result.CarsOut = parkingLot.TotalOut

			// send data down the channels
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

// NewInferModel reads DNN model and its configuration, sets its preferable target and backend and returns it.
// It returns error if either the model files failed to be read or setting the target or backend fails.
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

// frame is used to send video frames to upstream goroutines
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
		errChan <- frameRunner(framesChan, doneChan, resultsChan, pubChan, carNet)
	}()

	// open display window
	window := gocv.NewWindow(name)
	window.SetWindowProperty(gocv.WindowPropertyAutosize, gocv.WindowAutosize)
	defer window.Close()

	// prepare input image matrix
	img := gocv.NewMat()
	defer img.Close()

	// initialize the result pointers
	result := new(Result)

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
			// do nothing here
		default:
			// do nothing; just display latest results
		}
		// inference performance and print it
		gocv.PutText(&img, fmt.Sprintf("%s", result.Perf), image.Point{0, 25},
			gocv.FontHersheySimplex, 0.5, color.RGBA{255, 255, 255, 0}, 2)
		// inference results label
		gocv.PutText(&img, fmt.Sprintf("%s", result), image.Point{0, 45},
			gocv.FontHersheySimplex, 0.5, color.RGBA{255, 255, 255, 0}, 2)
		// Draw car centroids and label them with coordinates
		for id, _ := range result.Centroids {
			gocv.Circle(&img, result.Centroids[id].Point, 5, color.RGBA{0, 255, 0, 0}, 2)
			gocv.PutText(&img, fmt.Sprintf("%s", result.Centroids[id]),
				image.Point{X: result.Centroids[id].Point.X + 5, Y: result.Centroids[id].Point.Y},
				gocv.FontHersheySimplex, 0.5, color.RGBA{0, 255, 0, 0}, 2)
		}
		// show the image in the window, and wait 1 millisecond
		window.IMShow(img)

		// exit when ESC key is pressed
		if window.WaitKey(int(delay)) == 27 {
			break monitor
		}
	}
	// signal all goroutines to finish
	close(framesChan)
	close(doneChan)
	for range resultsChan {
		// collect any outstanding results
	}
	// wait for all goroutines to finish
	wg.Wait()
}
