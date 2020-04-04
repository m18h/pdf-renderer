FROM golang:alpine AS builder

RUN apk --no-cache add ca-certificates git
WORKDIR /usr/src/app

# libraries
RUN go get github.com/tools/godep

RUN dep ensure

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -v -a -installsuffix cgo -o app .

FROM scratch
FROM surnet/alpine-wkhtmltopdf:3.7-0.12.4-small

LABEL Author="Michael K. Essandoh <mexcon.mike@gmail.com>"

ENV GIN_MODE=release
ENV PORT=80

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

RUN mkdir -p /pdf-renderer
WORKDIR /pdf-renderer

EXPOSE 80

COPY --from=builder /usr/src/app/app .
ENTRYPOINT ["./app"]