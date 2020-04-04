FROM golang:alpine AS builder

ENV GOPATH=/go

RUN mkdir -p /go/src/app
WORKDIR /go/src/app

RUN apk --no-cache add ca-certificates git curl

RUN curl https://raw.githubusercontent.com/golang/dep/master/install.sh | sh

COPY . .

RUN dep ensure

RUN CGO_ENABLED=0 GOOS=linux go build -v -a -installsuffix cgo -o pdf-renderer .

FROM scratch
FROM surnet/alpine-wkhtmltopdf:3.10-0.12.5-small

LABEL Author="Michael K. Essandoh <Bearded0ne>"

ENV GIN_MODE=release
ENV PORT=80

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

RUN mkdir -p /app
WORKDIR /app

EXPOSE 80

COPY --from=builder /go/src/app/pdf-renderer .
ENTRYPOINT ["./pdf-renderer"]