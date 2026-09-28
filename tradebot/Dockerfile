# Paper-trading image (Alpaca). Contains tradebot and the LLM-free trendbot
# (run it with: --entrypoint /app/trendbot ... run --config /app/configs/trendbot.yaml). Robinhood mode needs Claude Code with a
# logged-in Robinhood MCP connection, so run that on a host (see docs/RUNBOOK.md).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tradebot ./cmd/tradebot && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trendbot ./cmd/trendbot

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/tradebot /app/tradebot
COPY --from=build /out/trendbot /app/trendbot
COPY configs /app/configs
ENTRYPOINT ["/app/tradebot"]
CMD ["run", "--config", "/app/configs/config.yaml", "--broker", "alpaca-paper"]
