FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/arara-agent-gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/arara-agent-gateway /usr/local/bin/arara-agent-gateway
ENV DATA_DIR=/data PORT=3000
EXPOSE 3000
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/arara-agent-gateway"]
