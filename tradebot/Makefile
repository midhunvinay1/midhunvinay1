BIN := bin/tradebot
CONFIG ?= configs/config.yaml

.PHONY: build test vet synth-backtest fetch backtest sweep paper live status halt resume \
	synth-intraday fetch-intraday backtest-intraday day-dry day-paper day-live rh-login rh-tools doctor

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

## ---- Intraday (short-term) mode ----
synth-intraday: build          ## pipeline smoke test on synthetic minute data
	$(BIN) synth-intraday --config $(CONFIG) --out data/minute-synth --days 250
	$(BIN) backtest-intraday --config $(CONFIG) --data data/minute-synth --out results/intraday-synth

fetch-intraday: build          ## ~1 GB per 40 symbols x 2 years of minute bars (gzip)
	$(BIN) fetch-intraday --config $(CONFIG) --start 2023-01-01 --out data/minute

backtest-intraday: build
	$(BIN) backtest-intraday --config $(CONFIG) --data data/minute --out results/intraday

day-dry: build
	$(BIN) day --config $(CONFIG) --broker dry-run

day-paper: build
	$(BIN) day --config $(CONFIG) --broker alpaca-paper

day-live: build
	$(BIN) day --config $(CONFIG) --broker robinhood-native

rh-login rh-tools doctor: build
	$(BIN) $@ --config $(CONFIG)
