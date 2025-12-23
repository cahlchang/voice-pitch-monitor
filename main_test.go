package main

import (
	"math"
	"testing"
	"time"
)

func TestFreqToNoteA4(t *testing.T) {
	n, cents := freqToNote(440.0)
	if n != "A4" {
		t.Fatalf("expected A4, got %s", n)
	}
	if math.Abs(cents) > 0.1 {
		t.Fatalf("expected cents near 0, got %.3f", cents)
	}
}

func TestNoteNameToFreq(t *testing.T) {
	f, ok := noteNameToFreq("A3")
	if !ok {
		t.Fatalf("expected ok")
	}
	if math.Abs(f-220.0) > 0.5 {
		t.Fatalf("expected ~220Hz, got %.3f", f)
	}
	if _, ok := noteNameToFreq("H#9"); ok {
		t.Fatalf("expected invalid note to fail")
	}
}

func TestBuildReferenceNotesRange(t *testing.T) {
	notes := buildReferenceNotes()
	if len(notes) == 0 {
		t.Fatalf("expected non-empty ref notes")
	}
	if notes[0] != "F#2" {
		t.Fatalf("expected first note F#2, got %s", notes[0])
	}
	if notes[len(notes)-1] != "F4" {
		t.Fatalf("expected last note F4, got %s", notes[len(notes)-1])
	}
}

func TestDetectPitchSine(t *testing.T) {
	sampleRate := 48000.0
	target := 220.0
	samples := make([]float32, 2048)
	for i := range samples {
		samples[i] = float32(0.8 * math.Sin(2*math.Pi*target*float64(i)/sampleRate))
	}
	freq, rms := detectPitch(samples, sampleRate)
	if rms <= silenceFloor {
		t.Fatalf("expected audible rms, got %.4f", rms)
	}
	if math.Abs(freq-target) > 2.0 {
		t.Fatalf("expected freq near %.1f, got %.2f", target, freq)
	}
}

func TestDetectPitchSilence(t *testing.T) {
	samples := make([]float32, 2048)
	freq, rms := detectPitch(samples, 48000)
	if freq != 0 {
		t.Fatalf("expected 0 freq for silence, got %.3f", freq)
	}
	if rms != 0 {
		t.Fatalf("expected 0 rms for silence, got %.6f", rms)
	}
}

func TestSmoothFreq(t *testing.T) {
	prev := 200.0
	sample := 400.0
	dt := 350 * time.Millisecond
	s := smoothFreq(prev, dt, sample)
	if s <= prev || s >= sample {
		t.Fatalf("expected smoothed value between prev and sample, got %.2f", s)
	}
	// At tau=350ms, a single step should move roughly 63% toward the sample.
	expected := prev + 0.63*(sample-prev)
	if math.Abs(s-expected) > 20 {
		t.Fatalf("expected near %.2f, got %.2f", expected, s)
	}
}
