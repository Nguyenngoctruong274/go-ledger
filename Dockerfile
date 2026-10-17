FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledgerd ./cmd/ledgerd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook-sink ./cmd/webhook-sink

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledgerd /out/webhook-sink /
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/ledgerd"]
