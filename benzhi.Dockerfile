FROM golang:1.26.3

WORKDIR /app

COPY go.mod ./
COPY . .

RUN go build ./...

CMD ["bash"]
