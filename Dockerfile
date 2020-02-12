FROM golang:alpine

WORKDIR /go/src/godhcperf
COPY . .
RUN go build -o main .

CMD ["./main"]
