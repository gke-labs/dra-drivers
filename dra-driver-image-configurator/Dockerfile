FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /controller ./cmd/controller
RUN CGO_ENABLED=0 go build -o /webhook ./cmd/webhook

FROM gcr.io/distroless/static:nonroot
COPY --from=build /controller /controller
COPY --from=build /webhook /webhook
