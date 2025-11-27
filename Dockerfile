FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY main.go .

RUN CGO_ENABLED=0 go build -ldflags '-extldflags "-static"' -o /spot-notifier main.go


FROM gcr.io/distroless/static

WORKDIR /app

COPY --from=builder /spot-notifier /app/

USER nonroot:nonroot

ENTRYPOINT ["/app/spot-notifier"]
