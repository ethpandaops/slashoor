FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X github.com/slashoor/slashoor/cmd.Version=${VERSION} -X github.com/slashoor/slashoor/cmd.GitCommit=${GIT_COMMIT} -X github.com/slashoor/slashoor/cmd.BuildTime=${BUILD_TIME}" \
    -o slashoor .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

RUN adduser -D -g '' slashoor
USER slashoor

WORKDIR /app

COPY --from=builder /app/slashoor .

ENTRYPOINT ["/app/slashoor"]
