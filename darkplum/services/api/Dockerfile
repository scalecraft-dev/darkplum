FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /darkplum-api ./cmd/

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /darkplum-api /darkplum-api

EXPOSE 8080
ENTRYPOINT ["/darkplum-api"]
