# Changelog

All notable changes to `zcode-gateway` will be documented in this file.

## [1.0.0] - 2026-11-07

### Added
- OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) compatible gateway for ZCode Start-Plan.
- Support for GLM-5.3 and GLM-5.3-Flash.
- Fine-grained reasoning effort control (`low`, `high`, `max`) via OpenAI `reasoning_effort` parameter or model ID suffix (`:low`, `:high`, `:max`).
- Automatic `reasoning_content` extraction in both streaming deltas and non-streaming responses.
- Automatic Windows desktop installation detection for device MID (`%APPDATA%\ZCode\.updaterId`).
- Automatic local token synchronizer (`sync-token.js`).
