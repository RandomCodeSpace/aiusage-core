// Package all assembles the registry of every adapter this project ships.
//
// It is a SEPARATE package from adapter on purpose (issue #73, decision 2).
// Every concrete adapter imports adapter for the contract types, so package
// adapter importing them back would be an import cycle; and a consumer who only
// wants to implement the interface, or to run one harness, should not pull
// fifteen file-format parsers in behind it. The contract package therefore keeps
// a zero-adapter dependency fan, and this package is where the fan-out lives.
//
// Importing it links every adapter into the binary. Build your own registry with
// adapter.NewRegistry when that is not what you want.
package all

import (
	"github.com/RandomCodeSpace/aiusage-core/adapter"
	"github.com/RandomCodeSpace/aiusage-core/adapter/agy"
	"github.com/RandomCodeSpace/aiusage-core/adapter/claudecode"
	"github.com/RandomCodeSpace/aiusage-core/adapter/clinecli"
	"github.com/RandomCodeSpace/aiusage-core/adapter/codex"
	"github.com/RandomCodeSpace/aiusage-core/adapter/copilot"
	"github.com/RandomCodeSpace/aiusage-core/adapter/crush"
	"github.com/RandomCodeSpace/aiusage-core/adapter/dsh"
	"github.com/RandomCodeSpace/aiusage-core/adapter/goose"
	"github.com/RandomCodeSpace/aiusage-core/adapter/hermes"
	"github.com/RandomCodeSpace/aiusage-core/adapter/kimicode"
	"github.com/RandomCodeSpace/aiusage-core/adapter/opencode"
	"github.com/RandomCodeSpace/aiusage-core/adapter/pi"
	"github.com/RandomCodeSpace/aiusage-core/adapter/qwencode"
	"github.com/RandomCodeSpace/aiusage-core/adapter/reasonix"
)

// Default returns a registry wired with every built-in adapter, in the order
// they were added to the project. A fresh registry per call: it holds no state
// and a caller is free to keep or discard it.
func Default() *adapter.Registry {
	return adapter.NewRegistry(
		claudecode.New(),
		codex.New(),
		copilot.New(),
		opencode.New(),
		hermes.New(),
		agy.New(),
		clinecli.New(),
		crush.New(),
		dsh.New(),
		goose.New(),
		kimicode.New(),
		// Pi and OpenClaw are two harnesses over one session format, so one
		// package serves both. They are separate registry entries because they
		// are separate tools: their rows must never be summed into one.
		pi.NewPi(),
		pi.NewOpenClaw(),
		qwencode.New(),
		reasonix.New(),
	)
}
