package main

import (
	"fmt"
	"image/color"
	"log"
	"math"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/gen2brain/malgo"
)

const (
	minPitchHz        = 70.0
	maxPitchHz        = 900.0
	silenceFloor      = 0.005
	silenceHold       = 3 * time.Second
	barFullScaleCents = 200.0 // ~B3 vs A3 difference fills the bar
	freqSmoothTau     = 350 * time.Millisecond
)

type deviceOption struct {
	Name string
	Info *malgo.DeviceInfo
}

type referenceState struct {
	mu   sync.RWMutex
	note string
	freq float64
}

func (r *referenceState) set(note string, freq float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note = note
	r.freq = freq
}

func (r *referenceState) get() (string, float64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.note, r.freq
}

type audioRunner struct {
	stop func()
	mu   sync.Mutex
}

func (r *audioRunner) replace(stop func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		r.stop()
	}
	r.stop = stop
}

func (r *audioRunner) shutdown() {
	r.replace(nil)
}

func main() {
	runtime.LockOSThread()
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		log.Printf("malgo: %s", message)
	})
	if err != nil {
		log.Fatalf("malgo init failed: %v", err)
	}
	defer ctx.Free()
	defer ctx.Uninit()

	a := app.NewWithID("voice-pitch-monitor")
	w := a.NewWindow("Pitch Monitor")
	w.Resize(fyne.NewSize(260, 140))
	w.SetFixedSize(true)
	prefs := a.Preferences()

	statusText := binding.NewString()
	_ = statusText.Set("Select input device")
	statusLabel := widget.NewLabelWithData(statusText)

	freqText := binding.NewString()
	_ = freqText.Set("-- Hz")
	freqLabel := canvas.NewText("-- Hz", theme.ForegroundColor())
	freqLabel.Alignment = fyne.TextAlignCenter
	freqLabel.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
	freqLabel.TextSize = 26.0

	noteText := binding.NewString()
	_ = noteText.Set("--")
	noteLabel := canvas.NewText("--", theme.ForegroundColor())
	noteLabel.Alignment = fyne.TextAlignCenter
	noteLabel.TextStyle = fyne.TextStyle{Bold: true}
	noteLabel.TextSize = 28.0

	// Binding updates -> canvas.Text
	freqText.AddListener(binding.NewDataListener(func() {
		val, _ := freqText.Get()
		freqLabel.Text = val
		freqLabel.Refresh()
	}))
	noteText.AddListener(binding.NewDataListener(func() {
		val, _ := noteText.Get()
		noteLabel.Text = val
		noteLabel.Refresh()
	}))

	devices, err := inputDevices(ctx)
	if err != nil {
		log.Fatalf("input devices: %v", err)
	}
	deviceNames := make([]string, len(devices))
	for i, d := range devices {
		deviceNames[i] = d.Name
	}

	runner := &audioRunner{}
	ref := &referenceState{}
	refNotes := buildReferenceNotes()
	defaultRef := "A3"
	if prefRef := prefs.StringWithFallback("last_ref_note", defaultRef); indexOf(refNotes, prefRef) >= 0 {
		defaultRef = prefRef
	}
	if freq, ok := noteNameToFreq(defaultRef); ok {
		ref.set(defaultRef, freq)
	} else if len(refNotes) > 0 {
		n := refNotes[0]
		freq, _ := noteNameToFreq(n)
		ref.set(n, freq)
	}
	refLabel := widget.NewLabel(fmt.Sprintf("Ref: %s", defaultRef))
	refSelect := widget.NewSelect(refNotes, func(n string) {
		if freq, ok := noteNameToFreq(n); ok {
			ref.set(n, freq)
			refLabel.SetText(fmt.Sprintf("Ref: %s", n))
			prefs.SetString("last_ref_note", n)
		}
	})
	refSelect.PlaceHolder = "Scroll & pick"
	if idx := indexOf(refNotes, defaultRef); idx >= 0 {
		refSelect.SetSelected(refNotes[idx])
	}

	pitchBar := NewPitchBar()

	lastDevice := prefs.String("last_device")
	deviceSelect := widget.NewSelect(deviceNames, func(name string) {
		selected := findDeviceByName(devices, name)
		if selected == nil {
			_ = statusText.Set("Device not found")
			return
		}
		prefs.SetString("last_device", name)
		_ = statusText.Set("Starting mic...")
		stop, err := startStream(ctx, selected.Info, ref, pitchBar, freqText, noteText, statusText)
		if err != nil {
			_ = statusText.Set(fmt.Sprintf("Error: %v", err))
			return
		}
		_ = statusText.Set(fmt.Sprintf("Listening on %s", selected.Name))
		runner.replace(stop)
	})
	deviceSelect.PlaceHolder = "Input device"
	if len(deviceNames) > 0 {
		if idx := indexOf(deviceNames, lastDevice); idx >= 0 {
			deviceSelect.SetSelected(lastDevice)
		} else {
			deviceSelect.SetSelected(deviceNames[0])
		}
	}

	leftCol := container.NewVBox(
		container.NewCenter(noteLabel),
		container.NewCenter(freqLabel),
		container.NewCenter(pitchBar),
	)
	rightCol := container.NewVBox(
		refLabel,
		refSelect,
	)

	content := container.NewVBox(
		deviceSelect,
		container.NewGridWithColumns(2, leftCol, rightCol),
		statusLabel,
	)
	w.SetContent(content)

	// Stop audio cleanly on window close or Ctrl+C.
	w.SetCloseIntercept(func() {
		runner.shutdown()
		a.Quit()
	})
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		runner.shutdown()
		w.Close()
	}()

	w.ShowAndRun()
}

func inputDevices(ctx *malgo.AllocatedContext) ([]deviceOption, error) {
	list, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, err
	}
	devs := make([]deviceOption, 0, len(list))
	for i := range list {
		info := list[i]
		infoCopy := info
		name := infoCopy.Name()
		if name == "" {
			name = "Unknown input"
		}
		devs = append(devs, deviceOption{
			Name: name,
			Info: &infoCopy,
		})
	}
	return devs, nil
}

func findDeviceByName(devs []deviceOption, name string) *deviceOption {
	for i := range devs {
		if devs[i].Name == name {
			return &devs[i]
		}
	}
	return nil
}

func startStream(ctx *malgo.AllocatedContext, info *malgo.DeviceInfo, ref *referenceState, bar *PitchBar, freqText, noteText, statusText binding.String) (func(), error) {
	config := malgo.DefaultDeviceConfig(malgo.Capture)
	config.Capture.Format = malgo.FormatF32
	config.Capture.Channels = 1
	config.SampleRate = chooseSampleRate(info)
	config.Capture.DeviceID = info.ID.Pointer()
	config.Alsa.NoMMap = 1

	samplesCh := make(chan []float32, 8)
	deviceCallbacks := malgo.DeviceCallbacks{
		Data: func(output, input []byte, frameCount uint32) {
			if len(input) == 0 {
				return
			}
			samples := bytesToFloat32Slice(input)
			buf := make([]float32, len(samples))
			copy(buf, samples)
			select {
			case samplesCh <- buf:
			default:
			}
		},
	}

	device, err := malgo.InitDevice(ctx.Context, config, deviceCallbacks)
	if err != nil {
		return nil, fmt.Errorf("init device: %w", err)
	}
	if err := device.Start(); err != nil {
		device.Uninit()
		return nil, fmt.Errorf("start device: %w", err)
	}

	stop := make(chan struct{})
	var lastDetected time.Time
	var lastRMS float64
	var smoothedFreq float64
	var lastSmooth time.Time
	go func() {
		sampleRate := float64(config.SampleRate)
		for {
			select {
			case <-stop:
				return
			case buf := <-samplesCh:
				freq, rms := detectPitch(buf, sampleRate)
				if freq <= 0 {
					if rms < silenceFloor && !lastDetected.IsZero() && time.Since(lastDetected) < silenceHold {
						_ = statusText.Set(fmt.Sprintf("Holding (Level %.2f)", lastRMS))
						continue
					}
					lastDetected = time.Time{}
					smoothedFreq = 0
					lastSmooth = time.Time{}
					_ = freqText.Set("-- Hz")
					_ = noteText.Set("--")
					bar.SetDelta(0)
					_ = statusText.Set("Listening...")
					continue
				}
				lastDetected = time.Now()
				lastRMS = rms
				now := time.Now()
				if lastSmooth.IsZero() {
					smoothedFreq = freq
				} else {
					smoothedFreq = smoothFreq(smoothedFreq, now.Sub(lastSmooth), freq)
				}
				lastSmooth = now

				name, cents := freqToNote(smoothedFreq)
				_, refFreq := ref.get()
				if refFreq <= 0 {
					refFreq = 440.0
				}
				deltaCents := 1200 * math.Log2(smoothedFreq/refFreq)
				bar.SetDelta(deltaCents)
				_ = freqText.Set(fmt.Sprintf("%.1f Hz", smoothedFreq))
				_ = noteText.Set(fmt.Sprintf("%s (%+.0f¢)", name, cents))
				_ = statusText.Set(fmt.Sprintf("Level %.2f", rms))
			}
		}
	}()

	return func() {
		close(stop)
		_ = device.Stop()
		device.Uninit()
	}, nil
}

func chooseSampleRate(info *malgo.DeviceInfo) uint32 {
	for _, f := range info.Formats {
		if f.SampleRate > 0 {
			return f.SampleRate
		}
	}
	return 48000
}

func bytesToFloat32Slice(b []byte) []float32 {
	hdr := *(*reflect.SliceHeader)(unsafe.Pointer(&b))
	hdr.Len /= 4
	hdr.Cap /= 4
	return *(*[]float32)(unsafe.Pointer(&hdr))
}

func detectPitch(samples []float32, sampleRate float64) (float64, float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s)
	}
	mean := sum / float64(len(samples))

	var energy float64
	normalized := make([]float64, len(samples))
	for i, s := range samples {
		v := float64(s) - mean
		normalized[i] = v
		energy += v * v
	}
	rms := math.Sqrt(energy / float64(len(samples)))
	if rms < silenceFloor {
		return 0, rms
	}

	minLag := int(sampleRate / maxPitchHz)
	maxLag := int(sampleRate / minPitchHz)
	if maxLag >= len(normalized) {
		maxLag = len(normalized) - 1
	}

	var bestLag int
	var bestCorr float64
	for lag := minLag; lag <= maxLag; lag++ {
		var corr float64
		for i := 0; i < len(normalized)-lag; i++ {
			corr += normalized[i] * normalized[i+lag]
		}
		if corr > bestCorr {
			bestCorr = corr
			bestLag = lag
		}
	}
	if bestLag == 0 {
		return 0, rms
	}
	freq := sampleRate / float64(bestLag)
	if freq < minPitchHz || freq > maxPitchHz {
		return 0, rms
	}
	return freq, rms
}

func freqToNote(freq float64) (string, float64) {
	if freq <= 0 {
		return "--", 0
	}
	notes := []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}
	n := math.Round(12 * math.Log2(freq/440.0))
	midi := int(n) + 69
	octave := midi/12 - 1
	name := notes[midi%12]
	target := 440.0 * math.Pow(2, n/12)
	cents := 1200 * math.Log2(freq/target)
	return fmt.Sprintf("%s%d", name, octave), cents
}

func noteNameToFreq(name string) (float64, bool) {
	m, ok := parseNote(name)
	if !ok {
		return 0, false
	}
	return midiToFreq(m), true
}

func midiToFreq(m int) float64 {
	return 440.0 * math.Pow(2, float64(m-69)/12)
}

func parseNote(s string) (int, bool) {
	if len(s) < 2 {
		return 0, false
	}
	base := s[:len(s)-1]
	octChar := s[len(s)-1]
	octave := int(octChar - '0')
	if octave < 0 || octave > 9 {
		return 0, false
	}
	semitones := map[string]int{
		"C":  0,
		"C#": 1,
		"D":  2,
		"D#": 3,
		"E":  4,
		"F":  5,
		"F#": 6,
		"G":  7,
		"G#": 8,
		"A":  9,
		"A#": 10,
		"B":  11,
	}
	val, ok := semitones[base]
	if !ok {
		return 0, false
	}
	midi := (octave+1)*12 + val
	return midi, true
}

func midiToName(midi int) string {
	notes := []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}
	name := notes[midi%12]
	oct := midi/12 - 1
	return fmt.Sprintf("%s%d", name, oct)
}

func smoothFreq(prev float64, dt time.Duration, sample float64) float64 {
	if prev == 0 || dt <= 0 {
		return sample
	}
	alpha := 1 - math.Exp(-float64(dt)/float64(freqSmoothTau))
	return prev + alpha*(sample-prev)
}

func buildReferenceNotes() []string {
	notes := make([]string, 0)
	for m := 42; m <= 65; m++ { // F#2..F4
		notes = append(notes, midiToName(m))
	}
	return notes
}

func indexOf(list []string, target string) int {
	for i, v := range list {
		if v == target {
			return i
		}
	}
	return -1
}

type PitchBar struct {
	widget.BaseWidget
	delta float64
}

func NewPitchBar() *PitchBar {
	p := &PitchBar{}
	p.ExtendBaseWidget(p)
	return p
}

func (p *PitchBar) SetDelta(cents float64) {
	if cents > barFullScaleCents {
		cents = barFullScaleCents
	}
	if cents < -barFullScaleCents {
		cents = -barFullScaleCents
	}
	p.delta = cents
	p.Refresh()
}

func (p *PitchBar) CreateRenderer() fyne.WidgetRenderer {
	line := canvas.NewRectangle(color.NRGBA{R: 160, G: 160, B: 160, A: 180})
	pos := canvas.NewRectangle(color.NRGBA{R: 220, G: 80, B: 80, A: 220})
	neg := canvas.NewRectangle(color.NRGBA{R: 80, G: 140, B: 230, A: 220})
	return &pitchBarRenderer{
		bar:  p,
		line: line,
		pos:  pos,
		neg:  neg,
	}
}

type pitchBarRenderer struct {
	bar  *PitchBar
	line *canvas.Rectangle
	pos  *canvas.Rectangle
	neg  *canvas.Rectangle
}

func (r *pitchBarRenderer) Layout(size fyne.Size) {
	centerY := size.Height / 2
	r.line.Move(fyne.NewPos(0, centerY-1))
	r.line.Resize(fyne.NewSize(size.Width, 2))

	r.pos.Hide()
	r.neg.Hide()

	ratio := float32(math.Abs(r.bar.delta) / barFullScaleCents)
	if ratio > 1 {
		ratio = 1
	}
	height := ratio * (size.Height / 2)
	if r.bar.delta > 0 {
		r.pos.Show()
		r.pos.Resize(fyne.NewSize(size.Width, height))
		r.pos.Move(fyne.NewPos(0, centerY-height))
		intensity := uint8(120 + 120*ratio)
		r.pos.FillColor = color.NRGBA{R: 220, G: 60, B: 60, A: intensity}
		r.pos.Refresh()
	} else if r.bar.delta < 0 {
		r.neg.Show()
		r.neg.Resize(fyne.NewSize(size.Width, height))
		r.neg.Move(fyne.NewPos(0, centerY))
		intensity := uint8(120 + 120*ratio)
		r.neg.FillColor = color.NRGBA{R: 60, G: 120, B: 220, A: intensity}
		r.neg.Refresh()
	}
}

func (r *pitchBarRenderer) MinSize() fyne.Size {
	return fyne.NewSize(28, 100)
}

func (r *pitchBarRenderer) Refresh() {
	r.Layout(r.bar.Size())
	canvas.Refresh(r.bar)
}

func (r *pitchBarRenderer) Destroy() {}

func (r *pitchBarRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.pos, r.neg, r.line}
}
