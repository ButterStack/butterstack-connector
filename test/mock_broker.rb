# frozen_string_literal: true

# A minimal stand-in for the ButterStack side of the Connector protocol.
#
# This is NOT the broker. The real broker is a dedicated Rack endpoint at
# /connect on the Rails app, with hashed-token auth, a Redis-routed command and
# result path, and explicit tenant scoping -- none of which exists yet and none
# of which this file models. What this models is exactly the surface the seven
# drills need: the /connect upgrade, the header-only token check, the frame
# exchange, and the ability to revoke a token or drop a connection on demand.
#
# It does implement, faithfully, the four rules the drills exist to prove:
#
#   1. The upgrade is refused before any per-connection state is allocated when
#      the token is absent, malformed, revoked, or wrong.
#   2. A token supplied as a query parameter is refused, and there is no code
#      path anywhere in this file that reads a token from a query string.
#   3. Tokens are stored as the SHA-256 digest of the secret segment and are
#      compared in constant time. The plaintext token is never held.
#   4. A result frame is matched against the issuing session's own outstanding
#      commands; a result carrying another session's command id is discarded.
require 'json'
require 'openssl'
require 'time'
require 'securerandom'
require 'socket'
require_relative 'support/ws'
require_relative 'support/tls'

class MockBroker
  HEARTBEAT_INTERVAL = 5 # seconds; the connector accepts 5..120, so this is negotiated for real

  # Session is one live connector connection.
  class Session
    attr_reader :id, :integration_id, :peer, :hello
    attr_accessor :last_seen

    def initialize(id:, integration_id:, peer:, hello:)
      @id = id
      @integration_id = integration_id
      @peer = peer
      @hello = hello
      @last_seen = Time.now
      @outstanding = {}
      @mutex = Mutex.new
      @cond = ConditionVariable.new
    end

    def issue(verb, args, deadline_ms:, max_bytes:, command_id: nil)
      id = command_id || SecureRandom.uuid
      @mutex.synchronize { @outstanding[id] = nil }
      frame = {
        type: 'command', id: id, verb: verb, args: args,
        deadline_ms: deadline_ms, max_bytes: max_bytes
      }
      @peer.send_text(JSON.generate(frame))
      id
    end

    def await(id, timeout)
      deadline = Time.now + timeout
      @mutex.synchronize do
        loop do
          v = @outstanding[id]
          return v if v

          remaining = deadline - Time.now
          return nil if remaining <= 0

          @cond.wait(@mutex, remaining)
        end
      end
    end

    # issue_unregistered sends a command frame WITHOUT recording its id, so the
    # connector's answer arrives as a result this session never asked for. That
    # is exactly the shape of a result frame belonging to another session, and
    # it drives the broker's real discard path rather than a simulated one.
    def issue_unregistered(verb, args, deadline_ms:, max_bytes:)
      id = SecureRandom.uuid
      @peer.send_text(JSON.generate({
                                      type: 'command', id: id, verb: verb, args: args,
                                      deadline_ms: deadline_ms, max_bytes: max_bytes
                                    }))
      id
    end

    # deliver returns true when the result belonged to this session.
    def deliver(result)
      @mutex.synchronize do
        return false unless @outstanding.key?(result['id'])

        @outstanding[result['id']] = result
        @cond.broadcast
        true
      end
    end

    def known_command?(id)
      @mutex.synchronize { @outstanding.key?(id) }
    end
  end

  attr_reader :port, :refusals, :received_frames, :discarded_results

  def initialize(host: 'localhost', tls_dir:, logger: nil)
    @host = host
    @tls = MockTLS.generate(tls_dir, host)
    @logger = logger
    @tokens = {}            # digest => { integration_id:, revoked: }
    @sessions = {}          # session id => Session
    @mutex = Mutex.new
    @refusals = []          # every refused upgrade, with a stable reason
    @received_frames = []   # raw inbound frame text, for the egress assertions
    @discarded_results = [] # cross-session result frames
    @partition_until = nil
    @port = nil
    @running = false
  end

  # register_token stores only the SHA-256 digest of the secret segment, which
  # is the pattern the design specifies and the opposite of the nearest
  # precedent in the app (Integration#webhook_token is stored reversibly).
  def register_token(token, integration_id:)
    @mutex.synchronize do
      @tokens[digest_of(token)] = { integration_id: integration_id, revoked: false }
    end
    token
  end

  # revoke marks the token dead and closes its live sessions, which is the
  # server half of "revoking on our side closes the socket within one heartbeat;
  # the studio's credentials are untouched".
  def revoke(token)
    d = digest_of(token)
    victims = []
    @mutex.synchronize do
      @tokens[d][:revoked] = true if @tokens.key?(d)
      victims = @sessions.values.select { |s| s.integration_id == integration_for(d) }
      victims.each { |s| @sessions.delete(s.id) }
    end
    victims.each do |s|
      s.peer.send_close(4401, 'token revoked')
      s.peer.close
    end
    victims.size
  end

  def start
    @server = TCPServer.new('127.0.0.1', @port || 0)
    @port = @server.addr[1]
    ctx = OpenSSL::SSL::SSLContext.new
    ctx.cert = @tls.server_cert
    ctx.key = @tls.server_key
    ctx.min_version = OpenSSL::SSL::TLS1_2_VERSION
    @ssl = OpenSSL::SSL::SSLServer.new(@server, ctx)
    @ssl.start_immediately = false
    @running = true
    @accept_thread = Thread.new { accept_loop }
    self
  end

  # stop closes the listener so new connections are refused at TCP level. This
  # is the "staging stopped" drill; the connector must back off, not die.
  def stop
    @running = false
    begin
      @ssl&.close
    rescue StandardError
      nil
    end
    @accept_thread&.kill
    @accept_thread = nil
    close_all_sessions
    self
  end

  # partition simulates the home network dropping: live sockets die and new
  # connections are dropped mid-handshake for the duration.
  def partition(seconds)
    @mutex.synchronize { @partition_until = Time.now + seconds }
    close_all_sessions
  end

  def ca_pem_path
    @tls.ca_pem_path
  end

  def endpoint(path = '/connect')
    "wss://#{@host}:#{@port}#{path}"
  end

  def sessions
    @mutex.synchronize { @sessions.values.dup }
  end

  def online?
    !sessions.empty?
  end

  # wait_for_session blocks until a connector is connected, or times out.
  def wait_for_session(timeout)
    deadline = Time.now + timeout
    loop do
      s = sessions.first
      return s if s
      return nil if Time.now > deadline

      sleep 0.05
    end
  end

  def wait_for_offline(timeout)
    deadline = Time.now + timeout
    loop do
      return true if sessions.empty?
      return false if Time.now > deadline

      sleep 0.05
    end
  end

  # call issues one command to the connected session and waits for the result.
  # This is the shape Connector.call(integration, verb, args) will have; the
  # difference on the real side is that it wraps in an explicit tenant scope and
  # routes through Redis, neither of which is modelled here.
  def call(verb, args = {}, timeout: 10, deadline_ms: 8000, max_bytes: 0, session: nil)
    s = session || sessions.first
    raise 'no connector session; the integration is offline' unless s

    id = s.issue(verb, args, deadline_ms: deadline_ms, max_bytes: max_bytes)
    res = s.await(id, timeout)
    raise "timed out waiting for result of #{verb}" unless res

    res
  end

  # degraded_call is what a verb-dependent feature does when the connector is
  # offline: it records "needs connector" and moves on, rather than raising.
  def degraded_call(verb)
    return { 'status' => 'needs_connector', 'verb' => verb } if sessions.empty?

    call(verb)
  end

  private

  def digest_of(token)
    secret = token.to_s.split('_', 3)[2].to_s
    OpenSSL::Digest::SHA256.hexdigest(secret)
  end

  def integration_for(dig)
    @tokens.dig(dig, :integration_id)
  end

  def secure_equal?(a, b)
    return false unless a.bytesize == b.bytesize

    OpenSSL.fixed_length_secure_compare(a, b)
  end

  def close_all_sessions
    victims = @mutex.synchronize do
      v = @sessions.values
      @sessions = {}
      v
    end
    victims.each { |s| s.peer.close }
  end

  def log(msg)
    @logger&.call("[broker] #{msg}")
  end

  def accept_loop
    while @running
      begin
        raw = @ssl.accept
      rescue StandardError
        break unless @running

        next
      end
      Thread.new(raw) { |sock| handle(sock) }
    end
  end

  def handle(raw_sock)
    partitioned = @mutex.synchronize { @partition_until && Time.now < @partition_until }
    if partitioned
      begin
        raw_sock.close
      rescue StandardError
        nil
      end
      return
    end

    begin
      raw_sock.accept # TLS handshake
    rescue StandardError
      return
    end

    peer = MockWS::Peer.new(raw_sock)
    begin
      req = peer.read_request(Time.now + 10)
    rescue StandardError
      peer.close
      return
    end

    # ---- Refusals, all of which happen before any session state exists -------
    return refuse(peer, req, 404, 'not_found') unless req.path == '/connect'

    # A query string on the upgrade is refused outright. A connector is not a
    # browser and can set headers, so the only reason to put a credential in a
    # URL is a limitation that does not apply here -- and a URL is logged by
    # every proxy on the path.
    if req.query && !req.query.empty?
      reason = req.query.include?('token') ? 'query_string_token' : 'query_string'
      return refuse(peer, req, 400, reason)
    end
    return refuse(peer, req, 426, 'not_an_upgrade') unless req.header('upgrade').to_s.downcase == 'websocket'

    key = req.header('sec-websocket-key')
    return refuse(peer, req, 400, 'missing_key') if key.nil? || key.empty?

    token = req.bearer_token
    return refuse(peer, req, 401, 'missing_bearer_token') if token.nil? || token.empty?
    return refuse(peer, req, 401, 'malformed_token') unless token.match?(/\Absc_[a-z0-9-]+_[A-Za-z2-7]+\z/)

    dig = digest_of(token)
    record = @mutex.synchronize { @tokens.find { |k, _| secure_equal?(k, dig) }&.last }
    return refuse(peer, req, 401, 'unknown_token') if record.nil?
    return refuse(peer, req, 401, 'revoked_token') if record[:revoked]

    peer.accept_upgrade(key)
    serve(peer, record[:integration_id])
  rescue StandardError => e
    log("handler error: #{e.class}: #{e.message}")
    peer&.close
  end

  def refuse(peer, req, status, reason)
    @mutex.synchronize { @refusals << { reason: reason, status: status, target: req&.target, at: Time.now } }
    log("refused #{reason} (#{status}) target=#{req&.target}")
    peer.respond_and_close(status, reason)
    nil
  end

  def serve(peer, integration_id)
    hello_raw = peer.read_message(15)
    record_frame(hello_raw)
    hello = JSON.parse(hello_raw)
    raise 'expected hello' unless hello['type'] == 'hello'

    session = Session.new(id: SecureRandom.uuid, integration_id: integration_id, peer: peer, hello: hello)
    @mutex.synchronize { @sessions[session.id] = session }
    log("session #{session.id} up: #{hello['connector_id']} v#{hello['version']} verbs=#{hello['capabilities']&.size}")

    peer.send_text(JSON.generate({
                                   type: 'welcome', session_id: session.id,
                                   server_time: Time.now.utc.iso8601,
                                   min_supported_version: '0',
                                   heartbeat_interval: HEARTBEAT_INTERVAL
                                 }))

    loop do
      raw = peer.read_message(HEARTBEAT_INTERVAL * 6)
      record_frame(raw)
      frame = begin
        JSON.parse(raw)
      rescue JSON::ParserError
        next
      end
      case frame['type']
      when 'heartbeat'
        session.last_seen = Time.now
      when 'result'
        session.last_seen = Time.now
        # A result is matched against the issuing session's own outstanding
        # commands. A frame carrying a command id this session never received
        # is discarded, never dispatched by id alone.
        next if session.deliver(frame)

        @mutex.synchronize { @discarded_results << { session_id: session.id, command_id: frame['id'] } }
        log("discarded result for unknown command id #{frame['id']} on session #{session.id}")
      end
    end
  rescue MockWS::Closed, MockWS::Timeout, JSON::ParserError, StandardError => e
    log("session down: #{e.class}: #{e.message}")
  ensure
    @mutex.synchronize { @sessions.delete(session.id) } if session
    peer.close
  end

  def record_frame(raw)
    @mutex.synchronize { @received_frames << raw.to_s }
  end
end
