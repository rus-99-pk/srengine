export PROMPT_PATH=$(pwd)/internal/agent/prompt.txt
# export NAMESPACES=default,test-cascade
export OLLAMA_URL=http://localhost:11434
export OLLAMA_MODEL=qwen2.5:3b
export NOTIFIER_TYPE=stdout
export INTEGRATION=1

if [[ $1 ]]; then
    run="-run TestScenarios/$1"
fi

go clean -testcache
go test ./tests/integration/... -v -timeout=25m ${run}
