// steerd — a Unix-socket steering channel for agent loops: pause, redirect,
// or cancel a running loop in-band, with two-stage acknowledgements.
//
// version:    0.1.0
// author:     JaydenCJ
// license:    MIT
// repository: https://github.com/JaydenCJ/steerd
// keywords:   agent, steering, control-channel, unix-socket, pause, cancel, ndjson
//
// Zero runtime dependencies: standard library only.
module github.com/JaydenCJ/steerd

go 1.22
