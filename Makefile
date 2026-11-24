# uic — 统一工业通信网关：构建 / 发布 / SBOM 验收入口。
#
# 把 README §开发 的仪式固化成可复跑目标，并补齐 ADR-0001 要求的交叉编译
# 产物与 SBOM（docs/todo.md 的 G8 / G12 缺口）。核心逻辑是 `make check`，
# 也是 CI 的入口——流水线平台确定后只需调这一个目标。
#
# 设计约束（来自 ADR，改动前先读）：
#   - 零第三方依赖、零 CGO（ADR-0001 §10）：release 一律 CGO_ENABLED=0；
#     工具链只用 Go 自带（gofmt / go version -m），不引入任何第三方扫描器。
#   - go directive 固定 1.26.10，不可 bump（ADR-0004：GOSUMDB=off 会拒绝
#     工具链模块校验）。这里不显式设 GOTOOLCHAIN，沿用环境默认。
#   - 单二进制 ≤40MB、交叉编译三平台（ADR-0001 C1/C2）：-trimpath -ldflags="-s -w"。
#
# 用法：make help

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

GO   ?= go
BIN  := uic
CMD  := ./cmd/uic
DIST := dist

# 平台矩阵：ADR-0001 C2（linux/amd64, linux/arm64, windows/amd64）。
# 命名 uic-<os>-<arch>[.exe]，windows 补 .exe。
LINUX_AMD64 := $(DIST)/$(BIN)-linux-amd64
RELEASES := linux/amd64 linux/arm64 windows/amd64

.DEFAULT_GOAL := help

.PHONY: help build vet fmt test race check release sbom load clean

help: ## 列出可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## 构建全部包（CGO_ENABLED=0）
	CGO_ENABLED=0 $(GO) build ./...

vet: ## go vet 全仓
	$(GO) vet ./...

fmt: ## 检查 gofmt 格式（gofmt -l internal cmd tools；有输出即需 gofmt -w）
	@out=$$(gofmt -l internal cmd tools); \
	  if [ -n "$$out" ]; then \
	    echo "以下文件未格式化，运行 gofmt -w internal cmd tools："; echo "$$out"; exit 1; \
	  else echo "gofmt clean"; fi

test: ## 单元测试
	$(GO) test ./...

race: ## 竞态检测（ADR-0001 §10 强制 -race）
	$(GO) test -race -count=1 ./...

check: build vet fmt race ## 全套开发仪式（build+vet+fmt+race），CI 入口

release: ## 交叉编译单二进制到 dist/（CGO_ENABLED=0，三平台）
	@mkdir -p $(DIST)
	@for r in $(RELEASES); do \
	  os=$${r%/*}; arch=$${r#*/}; \
	  name=$(BIN); [ "$os" = "windows" ] && name=$(BIN).exe; \
	  echo "  release  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    $(GO) build -trimpath -ldflags="-s -w" -o $(DIST)/$$name-$$os-$$arch $(CMD); \
	done
	@echo "产物在 $(DIST)/"

$(LINUX_AMD64): ## 编译 linux/amd64 主二进制（release/sbom 的依赖）
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  $(GO) build -trimpath -ldflags="-s -w" -o $@ $(CMD)

sbom: $(LINUX_AMD64) ## 从产物提取依赖清单，生成 CycloneDX SBOM（零第三方工具）
	@$(GO) run ./tools/sbomgen -binary $(DIST)/$(BIN)-linux-amd64 -output $(DIST)/sbom.cdx.json
	@echo "SBOM: $(DIST)/sbom.cdx.json"

load: ## 负载测试：1000 点 / 50 设备（ADR-0001 C4/C5），报告吞吐/内存/协程
	@$(GO) run ./cmd/loadgen -devices 50 -points 20 -interval 100ms -duration 10s

clean: ## 删除构建产物
	rm -rf $(DIST)
