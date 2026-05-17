# SREngine Kubernetes Helm Chart

[![CI Status](https://github.com/rus-99-pk/srengine/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/rus-99-pk/srengine/actions/workflows/ci.yaml)
[![Releases downloads](https://img.shields.io/github/downloads/rus-99-pk/srengine/total.svg)](https://github.com/rus-99-pk/srengine/releases)
[![License](https://img.shields.io/github/license/rus-99-pk/srengine.svg)](https://github.com/rus-99-pk/srengine/blob/main/LICENSE)

**SREngine is an AI-powered SRE agent that automatically investigates Kubernetes alerts.**

<br/>

## Usage

[Helm](https://helm.sh) must be installed to use the charts. Please refer to Helm's [documentation](https://helm.sh/docs/) to get started.

Once Helm is set up properly, add the repository as follows:

```console
helm repo add srengine https://rus-99-pk.github.io/srengine
helm repo update
```

You can then run `helm search repo srengine` to see the available charts.

### Installing the Chart

To install the chart with the release name `srengine` using the default AI prompt and settings:

```console
helm install srengine srengine/srengine \
  --namespace srengine \
  --create-namespace
```

#### Using a Custom AI Prompt

If you want to customize the system prompt used by the AI, you can download the default template, edit it, and pass it during installation using `--set-file`:

```console
# 1. Download the default prompt
curl -o custom-prompt.txt https://raw.githubusercontent.com/rus-99-pk/srengine/main/internal/agent/prompt.txt

# 2. Edit custom-prompt.txt to your needs, then install:
helm install srengine srengine/srengine \
  --namespace srengine \
  --create-namespace \
  --set-file prompt=./custom-prompt.txt
```

### Uninstalling the Chart

To uninstall/delete the `srengine` deployment:

```console
helm uninstall srengine --namespace srengine
```

## Docker Images

Docker images for `srengine` are automatically built and published. They are available in the [GitHub Container Registry (ghcr.io)](https://github.com/rus-99-pk/srengine/pkgs/container/srengine).

## Contributing

The source code of the `srengine` application and its [Helm](https://helm.sh) chart can be found on GitHub: <https://github.com/rus-99-pk/srengine/>

We'd love to have you contribute! Please open an issue or submit a pull request if you have any improvements or bug fixes.

## License

This project is licensed under the [MIT License](https://github.com/rus-99-pk/srengine/blob/main/LICENSE).
