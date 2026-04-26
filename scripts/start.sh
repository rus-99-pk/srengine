export PROMPT_PATH=./internal/agent/prompt.txt \
export NAMESPACES=default,test-cascade \
export OLLAMA_URL=http://localhost:11434 \
export OLLAMA_MODEL=qwen2.5:3b \
export NOTIFIER_TYPE=stdout \

go run ./cmd/agent
