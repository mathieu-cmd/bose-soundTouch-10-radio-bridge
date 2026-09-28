# Bose SoundTouch radio bridge — voor Railway
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bose-cloud-bridge ./cmd/bose-cloud-bridge

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /bose-cloud-bridge /bose-cloud-bridge
ENV PORT=8080
EXPOSE 8080
CMD ["/bose-cloud-bridge"]
