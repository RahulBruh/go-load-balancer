# Builds both binaries into one small image; pick one with the command.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lb ./cmd/lb \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demo-backend ./cmd/demo-backend

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/lb"]
CMD ["-config", "/etc/lb/config.yaml"]
