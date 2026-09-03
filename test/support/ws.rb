# frozen_string_literal: true

# Minimal server-side RFC 6455 implementation for the drill harness.
#
# This is deliberately an independent implementation from the Go client in
# internal/wsclient: the two were written from the RFC rather than from each
# other, so a handshake or masking mistake on either side shows up as a failed
# drill instead of two halves of the same bug agreeing with one another.
#
# Scope: text frames, ping/pong, close, client-side masking. No extensions, no
# compression, no fragmentation on the server's send path.
require 'openssl'
require 'base64'
require 'socket'

module MockWS
  GUID = '258EAFA5-E914-47DA-95CA-5AB0DC85B11C'

  MAX_MESSAGE_BYTES = 1 << 20
  MAX_CONTROL_BYTES = 125

  OP_CONTINUATION = 0x0
  OP_TEXT         = 0x1
  OP_BINARY       = 0x2
  OP_CLOSE        = 0x8
  OP_PING         = 0x9
  OP_PONG         = 0xA

  Closed  = Class.new(StandardError)
  Timeout = Class.new(StandardError)

  # accept_key computes base64(sha1(key + GUID)).
  def self.accept_key(client_key)
    Base64.strict_encode64(OpenSSL::Digest::SHA1.digest(client_key.to_s + GUID))
  end

  # Request is a parsed HTTP upgrade request.
  Request = Struct.new(:method, :target, :path, :query, :headers, keyword_init: true) do
    def header(name)
      headers[name.downcase]
    end

    # bearer_token returns the token from the Authorization header, or nil.
    # There is no fallback to a query parameter anywhere in this file; the
    # broker's rejection of a query-string token depends on there being no
    # second place to look.
    def bearer_token
      v = header('authorization').to_s
      return nil unless v.start_with?('Bearer ')

      v.delete_prefix('Bearer ').strip
    end
  end

  # Peer wraps one accepted socket.
  class Peer
    attr_reader :sock

    def initialize(sock)
      @sock = sock
      @buf = +''
      @write_mutex = Mutex.new
    end

    # read_request parses the HTTP request line and headers.
    def read_request(deadline)
      raw = +''
      raw << read_exact(1, deadline) until raw.end_with?("\r\n\r\n")
      head, = raw.split("\r\n\r\n", 2)
      lines = head.split("\r\n")
      method, target, = lines.shift.to_s.split(' ')
      path, query = target.to_s.split('?', 2)
      headers = {}
      lines.each do |l|
        k, v = l.split(':', 2)
        next if k.nil? || v.nil?

        headers[k.strip.downcase] = v.strip
      end
      Request.new(method: method, target: target, path: path, query: query, headers: headers)
    end

    def write_raw(str)
      @write_mutex.synchronize { @sock.write(str) }
    end

    # respond_and_close writes a plain HTTP response. Used for every refusal, so
    # a refused upgrade never reaches the frame layer.
    def respond_and_close(status, body = '')
      text = {
        400 => 'Bad Request', 401 => 'Unauthorized', 403 => 'Forbidden',
        426 => 'Upgrade Required', 429 => 'Too Many Requests'
      }.fetch(status, 'Error')
      write_raw("HTTP/1.1 #{status} #{text}\r\n" \
                "Content-Type: text/plain\r\n" \
                "Content-Length: #{body.bytesize}\r\n" \
                "Connection: close\r\n\r\n#{body}")
      close
    rescue StandardError
      nil
    end

    def accept_upgrade(key)
      write_raw("HTTP/1.1 101 Switching Protocols\r\n" \
                "Upgrade: websocket\r\n" \
                "Connection: Upgrade\r\n" \
                "Sec-WebSocket-Accept: #{MockWS.accept_key(key)}\r\n\r\n")
    end

    def send_text(str)
      write_frame(OP_TEXT, str.to_s.dup.force_encoding(Encoding::BINARY))
    end

    def send_ping(payload = '')
      write_frame(OP_PING, payload)
    end

    def send_close(code = 1000, reason = '')
      payload = [code].pack('n') + reason.to_s
      payload = payload.byteslice(0, MAX_CONTROL_BYTES)
      write_frame(OP_CLOSE, payload)
    rescue StandardError
      nil
    end

    # read_message reassembles one text message, answering pings inline.
    # Returns the payload string, or raises Closed / Timeout.
    def read_message(timeout)
      deadline = Time.now + timeout
      msg = +''
      loop do
        fin, opcode, payload = read_frame(deadline)
        case opcode
        when OP_PING then write_frame(OP_PONG, payload)
        when OP_PONG then nil
        when OP_CLOSE then raise Closed, 'client sent close'
        when OP_BINARY then raise Closed, 'binary frame'
        when OP_TEXT, OP_CONTINUATION
          raise Closed, 'message too large' if msg.bytesize + payload.bytesize > MAX_MESSAGE_BYTES

          msg << payload
          return msg.force_encoding(Encoding::UTF_8) if fin
        else
          raise Closed, "unknown opcode #{opcode}"
        end
      end
    end

    def close
      @sock.close
    rescue StandardError
      nil
    end

    def closed?
      @sock.closed?
    end

    private

    def write_frame(opcode, payload)
      payload = payload.to_s.dup.force_encoding(Encoding::BINARY)
      n = payload.bytesize
      header = +''
      header << (0x80 | opcode).chr
      # A server never masks (RFC 6455 5.1).
      if n <= 125
        header << n.chr
      elsif n <= 0xFFFF
        header << 126.chr << [n].pack('n')
      else
        header << 127.chr << [n].pack('Q>')
      end
      write_raw(header + payload)
    end

    def read_frame(deadline)
      h = read_exact(2, deadline).bytes
      fin = (h[0] & 0x80) != 0
      raise Closed, 'reserved bits set' if (h[0] & 0x70) != 0

      opcode = h[0] & 0x0F
      masked = (h[1] & 0x80) != 0
      len = h[1] & 0x7F
      len = read_exact(2, deadline).unpack1('n') if len == 126
      len = read_exact(8, deadline).unpack1('Q>') if len == 127

      control = (opcode & 0x8) != 0
      raise Closed, 'fragmented control frame' if control && !fin
      raise Closed, 'oversized control frame' if control && len > MAX_CONTROL_BYTES
      raise Closed, 'frame too large' if len > MAX_MESSAGE_BYTES
      # RFC 6455 6.1: a client MUST mask. An unmasked client frame is a
      # protocol error, and asserting it here is a real check on the Go side.
      raise Closed, 'client frame was not masked' unless masked

      mask = read_exact(4, deadline).bytes
      payload = read_exact(len, deadline)
      unmasked = +''
      payload.bytes.each_with_index { |b, i| unmasked << (b ^ mask[i % 4]).chr }
      [fin, opcode, unmasked.force_encoding(Encoding::BINARY)]
    end

    def read_exact(n, deadline)
      return +'' if n.zero?

      while @buf.bytesize < n
        remaining = deadline - Time.now
        raise Timeout, 'read timed out' if remaining <= 0

        begin
          @buf << @sock.read_nonblock([n - @buf.bytesize, 16_384].max)
        rescue IO::WaitReadable, OpenSSL::SSL::SSLErrorWaitReadable
          raise Timeout, 'read timed out' unless IO.select([@sock.to_io], nil, nil, remaining)

          retry
        rescue IO::WaitWritable, OpenSSL::SSL::SSLErrorWaitWritable
          raise Timeout, 'write-wait timed out' unless IO.select(nil, [@sock.to_io], nil, remaining)

          retry
        rescue EOFError, Errno::ECONNRESET, IOError, OpenSSL::SSL::SSLError
          raise Closed, 'socket closed'
        end
      end
      out = @buf.byteslice(0, n)
      @buf = @buf.byteslice(n, @buf.bytesize - n) || +''
      out
    end
  end
end
