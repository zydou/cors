FROM golang:1.26-alpine AS build

WORKDIR /go/src/app
COPY . .

RUN CGO_ENABLED=0 go build -o /go/bin/cors main.go

FROM gcr.io/distroless/static-debian12:nonroot
ENV PORT=3000
COPY --from=build /go/bin/cors /
CMD ["/cors"]
