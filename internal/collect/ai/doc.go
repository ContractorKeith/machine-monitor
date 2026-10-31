// Package ai registers collect probes for loaded AI models: Ollama, LM Studio, ComfyUI, and
// GPU processes such as vLLM, SGLang and llama.cpp. They run every 5th sample, as upstream's
// agents do, and fill report.Report.AI. See package collect for the probe contract.
package ai
