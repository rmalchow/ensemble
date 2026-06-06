// AudioSink pure-Go runtime registry in one binary (no cgo, no build tags):
// precise = direct kernel ioctl ALSA on /dev/snd/pcmC*D*p; coarse = exec
// aplay/pw-play; Render=false if neither works. Leaf package. (Package doc lives
// on sink.go; this clause matches the sibling files' "package audio" — the import
// path is internal/audio/sink.)
package audio
