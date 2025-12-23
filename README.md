# Voice Pitch Monitor (Go)

Minimal widget that shows your microphone pitch in real time. Pick an input device, see frequency and musical note update continuously, and compare against a reference note with an up/down deviation bar.

## Requirements
- Go 1.22+ (repo Go version: `go 1.23.4`)
- No additional native deps (uses embedded miniaudio via `malgo`)

## Run
```bash
go run .
```

## Notes
- The window is small and fixed-size so you can park it on a screen edge; always-on-top behavior depends on the window manager.
- If you change input devices, select a different entry from the dropdown to restart the stream.
