BIN := bin/tradebot
CONFIG ?= configs/config.yaml

.PHONY: build test vet synth-backtest fetch backtest sweep paper live status halt resume

build:
	go build -trimpath -o $(BIN) ./cmd/tradebot

test:
	go test ./...

vet:
	go vet ./...

synth-backtest: build          ## pipeline smoke test on synthetic data (numbers are meaningless)
	$(BIN) synth --config $(CONFIG) --out data/synth
	$(BIN) backtest --config $(CONFIG) --data data/synth --out results/synth

fetch: build                   ## download real daily bars from Alpaca
	$(BIN) fetch --config $(CONFIG) --start 2012-01-01 --out data/real

backtest: build
	$(BIN) backtest --config $(CONFIG) --data data/real --out results/real

sweep: build
	$(BIN) sweep --config $(CONFIG) --data data/real --split 2020-01-01

paper: build
	$(BIN) run --config $(CONFIG) --broker alpaca-paper

live: build
	$(BIN) run --config $(CONFIG) --broker robinhood

status halt resume: build
	$(BIN) $@ --config $(CONFIG)
