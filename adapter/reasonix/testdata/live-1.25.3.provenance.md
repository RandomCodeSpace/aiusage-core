# Reasonix 1.25.3 daily ledger capture

`live-1.25.3.jsonl` contains two actual appended records from Reasonix v1.25.3 on Linux amd64. The adapter reads this content-free daily stats ledger. Session transcripts are excluded.

The installed native executable reported `reasonix v1.25.3`. Its SHA-256 was `bff784b0065fd6eafd96bdbe87ed3a32718e4126f0fd78a6d5a76824ed806896` before and after capture. Go build information identifies module revision `69b09d419d0d7022d609e8ab36de6838ca7fabe3`, with `vcs.modified=false`; upstream tag `v1.25.3` resolves to that same commit.

The first request ran from 2026-09-12T16:12:22.084925Z through 2026-09-12T16:12:23.252934Z and exited zero. The second ran from 2026-09-12T16:14:58.406613Z through 2026-09-12T16:14:59.374048Z and exited zero. These are capture execution times; each fixture row retains its writer timestamp.

A private temporary directory held a copied configuration, empty workspace and separate state. Both requests used the existing configured Ollama provider and `ollama/gemma4:31b-cloud`. The provider key was read and injected into the child environment in memory. No credential, configuration file, raw response or session transcript is included in the fixture. Both bounded requests used the same arithmetic prompt:

```sh
REASONIX_HOME="$capture/home" REASONIX_STATE_HOME="$capture/state" \
  reasonix -p --model ollama --output-format json --allowed-tools '' \
  'Reply with only the result of 23 plus 19. Do not invoke tools.'
```

The actual command also received the existing provider key through `OLLAMA_LOCAL_DUMMY_KEY`. The child ran in `$capture/workspace` in its own process group, with a 45 second timeout and a five second termination wait. Private output files used mode 0600. Neither invocation needed termination.

Before the second request, the stats ledger was 319 bytes, SHA-256 `01aedd09d80c6181914543805879a7fb8f17f688de0e27bf6eaa0fad61d565cd`. Afterward it was 638 bytes, SHA-256 `c51bddc7d700c739b25f586957e900d4bf3309ba045f60bdb0484256b407a3c2`. The original 319-byte prefix was unchanged. The committed fixture is byte-identical to that resulting ledger. No field substitution was needed: the surface contains counters, timestamps, model/provider identifiers and status flags, with no user content or paths.

`TestLive1253LedgerPrefixReplayAndAppend` applies the actual first prefix, repeats it, appends the actual second row, then replays the complete source. It asserts exactly two ledger inserts, stable distinct keys, the complete token tuple and times, no invented cost/session/project, unchanged source bytes and permissions, and no source sidecars. Both rows carry input 5088, output 3 and total 5091. The equal tuples with distinct timestamps prove separate request identity; existing August fixtures retain the decreasing-counter case.

## Versioned writer contract

The native binary's exact revision provides the reference, rather than an inferred universal schema:

- [Stats record fields and append implementation](https://github.com/esengine/DeepSeek-Reasonix/blob/69b09d419d0d7022d609e8ab36de6838ca7fabe3/internal/stats/record.go#L39) declares `ts` without `omitempty`. Model, source, prompt, completion, reasoning, cache counters, total and usage source all have `omitempty`. The writer marshals one record and appends it with a newline under its own append lock.
- [Recorder mapping](https://github.com/esengine/DeepSeek-Reasonix/blob/69b09d419d0d7022d609e8ab36de6838ca7fabe3/internal/stats/recorder.go#L219) assigns `Usage.ReasoningTokens` to `record.Reasoning` separately from completion and total. Omitted reasoning therefore represents zero in this version's writer. `TestVersionedWriterReasoningMapping` is a synthetic nonzero mapping test tied to that source; it is not a live reasoning observation.
- [Provider usage contract](https://github.com/esengine/DeepSeek-Reasonix/blob/69b09d419d0d7022d609e8ab36de6838ca7fabe3/internal/provider/provider.go#L698) explicitly defines reasoning as a subset of completion. Its SHA-256 is `cfe7dfaabcf2fefaaae52aba1c2b4490d05c3e934fd0fa9c625e3fe144f4d391`. The [OpenAI-compatible mapping](https://github.com/esengine/DeepSeek-Reasonix/blob/69b09d419d0d7022d609e8ab36de6838ca7fabe3/internal/provider/openai/openai.go#L1127) retains completion and total, uses prompt plus completion only as the missing-total fallback, and maps nested reasoning separately without adding it. Its SHA-256 is `12581422eee3cb495354040d0cb717e26a6ea6ec4e224d0ac209e7f60cd63bdd`. Both sources match the exact captured native revision.
- The same recorder accepts request-only usage and writes turn-completion markers. It can also record guardian usage with an empty usage source. Missing optional counters or usage source alone cannot prove drift.

The adapter identifies a current record by a nonempty `usage_source`, or an explicitly present `cost_complete` or `display_complete`. For those rows, a missing, renamed, null, empty, wrongly typed or malformed `ts` is diagnosed and the checkpoint is withheld. The diagnostic wraps `adapter.ErrSourceFormat`. Complete JSON with incompatible field types or no recognized stats fields receives the same classification and held checkpoint. Valid neighboring events remain usable and deduplicate on retry. `TestCurrentLedgerTimestampAnchorHoldsCheckpoint` proves rejection, prefix insertion and recovery. Older unmarked records retain the documented mtime/total fallback. `TestCurrentWriterOptionalFieldsAndLegacyFallback` pins the optional-field and bookkeeping cases; `TestCurrentLedgerPartialEOFRemainsUnconsumed` pins ordinary torn-tail completion. `TestCurrentLedgerRejectsUnrecognizedCompleteObjects` rejects complete unknown shapes and preserves zero/request/turn records and an incomplete first record at EOF. Unmarked shapes cannot be conclusively identified as current by these optional fields and retain legacy tolerance.

## Limits

The capture does not contain nonzero reasoning or native vendor cost. Reasoning omission and mapping are established by the versioned writer source. The writer can serialize optional currency-qualified cost amounts when a quote exists; this adapter currently leaves those amounts unmapped and permits the pricing layer to value usage. This fixture verifies the captured unpriced Ollama ledger, not every provider's billing shape or monetary equivalence. It establishes no activity, session attribution or turn-context support because the chosen ledger carries none of those identities.
