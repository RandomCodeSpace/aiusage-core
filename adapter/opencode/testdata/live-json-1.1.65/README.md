# OpenCode native JSON capture

These files were emitted directly by OpenCode 1.1.65 during an isolated test using the authorized Ollama Cloud endpoint. They are not SQLite exports. Capture: 2026-09-12T16:58:49.835644+00:00 to 2026-09-12T16:58:53.725314+00:00; elapsed 3.89 seconds, successful exit. Platform: Linux x86_64.

The [official release](https://github.com/anomalyco/opencode/releases/tag/v1.1.65) resolves to commit `34ebe814ddd130a787455dda089facb23538ca20`. The downloaded `opencode-linux-x64.tar.gz` matched the release SHA256 `264d236b37a539bc235ab5d67b3e4fd082844775353e5153514dc105e240ef6b`. Its executable reported `1.1.65` and retained SHA256 `f8c0ff3e406b0448a4ed5df07c316e89f6fb6848da2ffb9879fdf6671e8acd4e` before and after the test. The release uses the [MIT license](https://github.com/anomalyco/opencode/blob/v1.1.65/LICENSE). Only the self-contained executable was extracted into a temporary directory.

The exact [storage writer](https://github.com/anomalyco/opencode/blob/v1.1.65/packages/opencode/src/storage/storage.ts) and [session writer](https://github.com/anomalyco/opencode/blob/v1.1.65/packages/opencode/src/session/index.ts) establish the native directory and JSON format. The session records version `1.1.65`. All session, message, parent, part and call identifiers are unchanged. Temporary project paths and tool output were sanitized. User-message text and unrelated files were omitted.

Both assistant messages completed. Their input/output/reasoning/cache-read/cache-write/total counts are `589/46/0/2112/0/2747` and `143/6/0/2688/0/2837`. One `read` call completed. The JSON adapter currently imports usage only, so its activity output is zero; the part file records producer evidence without claiming adapter activity coverage. Source cost is zero; normalized cost remains unset for rate-based computation.

HOME, PWD, cwd, XDG roots, OpenCode test/config roots and TMPDIR were temporary. Only the temporary input file was authorized for the read tool; other tools were denied. The file and original source/config sentinels were unchanged. The syscall trace showed no write target outside the temporary capture root; its one relative write used a directory descriptor previously opened on temporary TMPDIR. The temporary credential config was removed after capture.

Live reasoning is zero. The constructed reasoning regression uses unchanged JavaScript from this exact executable and its bundled OpenAI-compatible SDK. Unlike current writers, 1.1.65 stores reasoning inside output. SDK input120/output30/reasoning30/cached30/total150 becomes native input90/output30/reasoning30/cache-read30/total150. The adapter removes the overlap only when a positive total exactly accounts for input, output and cache. It then reports input90/output0/reasoning30/cache-read30/total150. Missing or inconsistent totals remain ambiguous and retain additive behavior.

Frozen source hashes and sanitized fixture hashes:

| Native file | Source SHA256 | Fixture SHA256 |
| --- | --- | --- |
| `message/ses_f6971461fffeDLctuzpLpyhs6L/msg_0968eba98001rMDB7c7S5bO7or.json` | `efb4a3236a83dc5b477c6e9f44b3d554139c700ea3950b38dbc868ea9cc3c2e5` | `7c4daecafcf557815b3eb8343f70963bac775d6210e0a7285186959fbe02168e` |
| `message/ses_f6971461fffeDLctuzpLpyhs6L/msg_0968ec09f001UrEgRvui4lYkns.json` | `8c9074ff2ed4643afa411523ce4d63236f0525a3bfc36dca5cb60d0efcce1257` | `e41bc3743ea9918c56b0af72d845c45b27ec9626628697c7232e90f9d16b007c` |
| `part/msg_0968eba98001rMDB7c7S5bO7or/prt_0968ec04a0018TMQouqpOhKLzf.json` | `09cb0114693b4188d0e8472e6e17a83b2b189dbb62a730f2ad9fa55a9c659693` | `855f63121b337014cf6568d1da713a60e8050f1e0576507caf89b07e05bb483c` |
| `session/global/ses_f6971461fffeDLctuzpLpyhs6L.json` | `c5d1c460584e8681c5e70b8fa489473c84b05ffae6feb03b2f921fadd90c544d` | `01d551214195de9f7242119dd73294f4d349b8bb498fc4f4c1dc209950d56652` |
