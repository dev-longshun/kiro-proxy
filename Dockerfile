FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /kiro-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /kiro-proxy .
COPY web/ ./web/
COPY models.json ./

EXPOSE 8989
ENTRYPOINT ["/app/kiro-proxy"]
