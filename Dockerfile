# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=dev" -o /out/ai-concurrency-shaper .

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ai-concurrency-shaper /ai-concurrency-shaper
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/ai-concurrency-shaper"]
