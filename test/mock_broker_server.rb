#!/usr/bin/env ruby
# frozen_string_literal: true

# Standalone process wrapping MockBroker for container use in
# docker-compose.uat-connector.yml (connector.spec.js, issue #1574/#1575 UAT).
#
# `mock_broker.rb` is NOT the broker -- see its header comment and
# `test/README.md` "What the mock broker is not". The real broker is a
# dedicated Rack endpoint at /connect on the Rails app, with hashed-token
# auth, a Redis-routed command/result path, and explicit tenant scoping. What
# runs here is the same drill stand-in `drills.rb` uses, given two things a
# container needs that a same-process test harness does not:
#
#   1. a fixed, published wss:// listener (rather than loopback + an ephemeral
#      port), with a server certificate whose SAN matches the compose hostname
#      the connector container dials; and
#   2. a small HTTP admin API, PLAIN HTTP (not TLS, not the /connect protocol
#      itself), so the Playwright test on the host can drive sessions/refusals/
#      revoke/partition without speaking the wire protocol. This is exactly
#      the same "test harness talks to the broker object directly" shape
#      drills.rb uses in-process -- just reachable over the network instead of
#      via a Ruby method call. It is reachable only on the compose default
#      network and the host-published admin port; it is not part of the
#      Connector protocol and a real broker exposes nothing like it.
#
# Env:
#   BROKER_TOKEN            required: the connector token to register
#   BROKER_INTEGRATION_ID   required: the integration id segment of that token
#   BROKER_HOST             hostname baked into the server cert's SAN
#                            (default 'mock-broker' -- the compose service name
#                            the connector dials)
#   BROKER_BIND             wss:// listen address (default '0.0.0.0')
#   BROKER_PORT             wss:// listen port (default 9443)
#   BROKER_ADMIN_BIND       admin HTTP listen address (default '0.0.0.0')
#   BROKER_ADMIN_PORT       admin HTTP listen port (default 9400)
#   BROKER_TLS_DIR          where the throwaway CA/cert are generated and
#                            where ca.pem is published for other containers to
#                            trust (default '/tls', a shared volume)
require 'json'
require 'socket'
require 'securerandom'
require 'fileutils'
require_relative 'mock_broker'

TOKEN = ENV.fetch('BROKER_TOKEN')
INTEGRATION_ID = ENV.fetch('BROKER_INTEGRATION_ID')
HOST = ENV['BROKER_HOST'] || 'mock-broker'
BIND = ENV['BROKER_BIND'] || '0.0.0.0'
PORT = (ENV['BROKER_PORT'] || '9443').to_i
ADMIN_BIND = ENV['BROKER_ADMIN_BIND'] || '0.0.0.0'
ADMIN_PORT = (ENV['BROKER_ADMIN_PORT'] || '9400').to_i
TLS_DIR = ENV['BROKER_TLS_DIR'] || '/tls'

LOGGER = ->(m) { warn(m) }

FileUtils.mkdir_p(TLS_DIR)
BROKER = MockBroker.new(host: HOST, tls_dir: TLS_DIR, logger: LOGGER, bind: BIND, port: PORT)
BROKER.register_token(TOKEN, integration_id: INTEGRATION_ID)
BROKER.start
warn("[mock-broker] wss listening on #{BIND}:#{BROKER.port}, cert CN/SAN=#{HOST}, integration=#{INTEGRATION_ID}")

# Publish the CA cert on the shared /tls volume so the connector container (and
# anything else that wants to verify this broker) can trust it. Not 0600: this
# is a public certificate, not a secret, and other containers need to read it.
CA_PEM_PATH = File.join(TLS_DIR, 'ca.pem')
FileUtils.cp(BROKER.ca_pem_path, CA_PEM_PATH)
File.chmod(0o644, CA_PEM_PATH)
warn("[mock-broker] CA published at #{CA_PEM_PATH}")

# ---------------------------------------------------------------------------
# Admin HTTP API -- plain HTTP, hand-rolled in the style of teamcity_stub.rb.
# No gems: this is a drill/UAT fixture, not a shipped service.
# ---------------------------------------------------------------------------
def respond_json(sock, status, body)
  json = JSON.generate(body)
  text = { 200 => 'OK', 400 => 'Bad Request', 404 => 'Not Found', 502 => 'Bad Gateway' }.fetch(status, 'Error')
  sock.write("HTTP/1.1 #{status} #{text}\r\n" \
             "Content-Type: application/json\r\n" \
             "Content-Length: #{json.bytesize}\r\n" \
             "Connection: close\r\n\r\n#{json}")
end

def respond_text(sock, status, body, content_type: 'text/plain')
  text = { 200 => 'OK', 404 => 'Not Found' }.fetch(status, 'Error')
  sock.write("HTTP/1.1 #{status} #{text}\r\n" \
             "Content-Type: #{content_type}\r\n" \
             "Content-Length: #{body.bytesize}\r\n" \
             "Connection: close\r\n\r\n#{body}")
end

def session_json(s)
  {
    'id' => s.id,
    'integration_id' => s.integration_id,
    'connector_id' => s.hello['connector_id'],
    'version' => s.hello['version'],
    'capabilities' => s.hello['capabilities'],
    'tool_versions' => s.hello['tool_versions'],
    'last_seen' => s.last_seen.utc.iso8601
  }
end

def handle_admin(sock)
  head = +''
  head << sock.readpartial(1) until head.end_with?("\r\n\r\n")
  lines = head.split("\r\n")
  method, target, = lines.shift.split(' ')
  headers = lines.each_with_object({}) do |l, h|
    k, v = l.split(':', 2)
    h[k.to_s.strip.downcase] = v.to_s.strip if k && v
  end
  body = +''
  if (len = headers['content-length'].to_i) > 0
    body << sock.read(len).to_s
  end
  path = target.split('?', 2).first
  params = body.empty? ? {} : JSON.parse(body)

  case [method, path]
  when ['GET', '/health']
    respond_json(sock, 200, { 'ok' => true })

  when ['GET', '/ca.pem']
    respond_text(sock, 200, File.read(CA_PEM_PATH), content_type: 'application/x-pem-file')

  when ['GET', '/status']
    sessions = BROKER.sessions
    respond_json(sock, 200, {
                   'online' => BROKER.online?,
                   'sessions' => sessions.map { |s| session_json(s) },
                   'refusals' => BROKER.refusals.map { |r| r.merge(at: r[:at].utc.iso8601) },
                   'discarded_results_count' => BROKER.discarded_results.size,
                   'discarded_results' => BROKER.discarded_results
                 })

  when ['POST', '/call']
    verb = params.fetch('verb')
    args = params['args'] || {}
    deadline_ms = params['deadline_ms'] || 8000
    max_bytes = params['max_bytes'] || 0
    begin
      result = BROKER.call(verb, args, deadline_ms: deadline_ms, max_bytes: max_bytes)
      respond_json(sock, 200, result)
    rescue StandardError => e
      respond_json(sock, 200, { 'error' => e.message })
    end

  when ['POST', '/degraded_call']
    verb = params.fetch('verb')
    respond_json(sock, 200, BROKER.degraded_call(verb))

  when ['POST', '/issue_unregistered']
    session = BROKER.sessions.first
    if session
      id = session.issue_unregistered(params.fetch('verb'), params['args'] || {}, deadline_ms: 5000, max_bytes: 0)
      respond_json(sock, 200, { 'ok' => true, 'id' => id })
    else
      respond_json(sock, 200, { 'error' => 'no connector session' })
    end

  when ['POST', '/revoke']
    closed = BROKER.revoke(TOKEN)
    respond_json(sock, 200, { 'ok' => true, 'closed' => closed })

  when ['POST', '/register']
    BROKER.register_token(TOKEN, integration_id: INTEGRATION_ID)
    respond_json(sock, 200, { 'ok' => true })

  when ['POST', '/partition']
    seconds = (params['seconds'] || 5).to_i
    BROKER.partition(seconds)
    respond_json(sock, 200, { 'ok' => true, 'seconds' => seconds })

  else
    respond_json(sock, 404, { 'error' => "no such admin route #{method} #{path}" })
  end
rescue KeyError, JSON::ParserError => e
  respond_json(sock, 400, { 'error' => e.message })
rescue StandardError => e
  respond_json(sock, 502, { 'error' => "#{e.class}: #{e.message}" })
ensure
  begin
    sock.close
  rescue StandardError
    nil
  end
end

admin_server = TCPServer.new(ADMIN_BIND, ADMIN_PORT)
warn("[mock-broker] admin HTTP listening on #{ADMIN_BIND}:#{ADMIN_PORT}")
admin_running = true
admin_thread = Thread.new do
  while admin_running
    begin
      sock = admin_server.accept
    rescue StandardError
      break
    end
    Thread.new(sock) { |s| handle_admin(s) }
  end
end

Signal.trap('TERM') { exit(0) }
Signal.trap('INT') { exit(0) }
admin_thread.join
