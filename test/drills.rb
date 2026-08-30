#!/usr/bin/env ruby
# frozen_string_literal: true

# The seven drills from the connector design note section 4.3, run entirely
# locally against the mock broker.
#
#   Four recovery drills:
#     R1  the studio's network drops           -> the connector reconnects
#     R2  the connector is stopped             -> we go offline and degrade
#     R3  our side is stopped and restarted    -> backoff, then reconnect
#     R4  the connector token is revoked       -> the socket closes within one
#                                                 heartbeat and the studio's own
#                                                 credentials are untouched
#   Three denial drills:
#     D1  an out-of-vocabulary verb            -> denied
#     D2  an in-vocabulary verb with an
#         out-of-scope argument                -> denied
#     D3  a query-string token on /connect     -> refused before session state
#
#   Plus the broker-side half of drill (f): a result frame carrying another
#   session's command id is discarded. The other half of (f) -- asserting that
#   tenant context is nil at the start of the next request on the same Puma
#   thread -- is Rails-side and is NOT covered here; see README "what this does
#   not prove".
#
# Usage:  CONNECTOR_BIN=./build/butterstack-connector ruby test/drills.rb
require 'fileutils'
require 'json'
require 'open3'
require 'tmpdir'
require_relative 'mock_broker'
require_relative 'support/teamcity_stub'

VERBOSE = ENV['VERBOSE'] == '1'

def log(msg)
  warn("  #{msg}") if VERBOSE
end

CONNECTOR_BIN = ENV.fetch('CONNECTOR_BIN', File.expand_path('../build/butterstack-connector', __dir__))
unless File.executable?(CONNECTOR_BIN)
  abort "drills: #{CONNECTOR_BIN} is missing or not executable. Run `make build` first."
end

# A connector token in the specified format: bsc_<integration id>_<base32 secret>.
# 52 base32 characters is 32 random bytes, which is why the design says a plain
# SHA-256 digest is the right storage and a slow KDF would buy nothing.
BASE32 = (('A'..'Z').to_a + ('2'..'7').to_a).freeze

def new_token(integration_id = 'intg7f3a')
  "bsc_#{integration_id}_#{Array.new(52) { BASE32.sample }.join}"
end

TOKEN = new_token
TEAMCITY_TOKEN = 'tc-local-token-do-not-egress'
P4_TICKET = 'p4-local-ticket-do-not-egress'

# ---------------------------------------------------------------------------
# Result plumbing
# ---------------------------------------------------------------------------
Result = Struct.new(:name, :title, :ok, :detail, :evidence, keyword_init: true)
RESULTS = []

def drill(name, title)
  started = Time.now
  detail = []
  ok = true
  begin
    yield ->(claim, cond, note = nil) {
      pass = !!cond
      ok &&= pass
      detail << "#{pass ? 'PASS' : 'FAIL'}  #{claim}#{note ? " (#{note})" : ''}"
    }
  rescue StandardError => e
    ok = false
    detail << "FAIL  raised #{e.class}: #{e.message}"
    detail.concat(e.backtrace.first(3).map { |l| "        #{l}" }) if VERBOSE
  end
  RESULTS << Result.new(name: name, title: title, ok: ok, detail: detail,
                        evidence: format('%.2fs', Time.now - started))
  warn("#{ok ? 'ok  ' : 'FAIL'} #{name}  #{title}")
  detail.each { |d| warn("       #{d}") } if VERBOSE || !ok
end

# ---------------------------------------------------------------------------
# Harness
# ---------------------------------------------------------------------------
WORK = Dir.mktmpdir('connector-drills-')
at_exit { FileUtils.remove_entry(WORK) if File.directory?(WORK) }

LOGGER = ->(m) { log(m) }

teamcity = TeamCityStub.new(token: TEAMCITY_TOKEN, logger: LOGGER).start
teamcity.add_build(9001, build_type_id: 'Uat_Build', number: '512', revision: '41337')

broker = MockBroker.new(tls_dir: File.join(WORK, 'tls'), logger: LOGGER)
broker.register_token(TOKEN, integration_id: 'intg7f3a')
broker.start

ARGV_LOG = File.join(WORK, 'p4-argv.log')

# The connector hands its p4 subprocess a deliberately minimal environment
# (PATH, HOME, P4PORT, P4USER, P4PASSWD and nothing else), which is the right
# hygiene and also means the harness cannot pass the fake p4 its log path
# through the environment. So connector.yml points at a wrapper with the path
# baked in, and the daemon stays unweakened for the sake of the test.
P4_WRAPPER = File.join(WORK, 'p4-wrapper')
File.write(P4_WRAPPER, <<~SH)
  #!/bin/sh
  FAKE_P4_ARGV_LOG='#{ARGV_LOG}' exec '#{File.expand_path('support/fake_p4', __dir__)}' "$@"
SH
File.chmod(0o755, P4_WRAPPER)
CONFIG_PATH = File.join(WORK, 'connector.yml')
LOG_DIR = File.join(WORK, 'logs')

def write_config(path, broker:, teamcity:, log_dir:)
  File.write(path, <<~YAML)
    endpoint: #{broker.endpoint}
    endpoint_ca_file: #{broker.ca_pem_path}
    token: #{TOKEN}
    connector_id: drill-harness
    log_dir: #{log_dir}
    max_concurrent: 4
    scopes:
      depot_scope:
        - //depot/game/
      allowed_build_types:
        - Uat_Build
    perforce:
      enabled: true
      binary: #{P4_WRAPPER}
      port: ssl:p4.lan:1666
      user: butterstack-ro
      ticket: #{P4_TICKET}
    teamcity:
      enabled: true
      url: #{teamcity.base_url}
      token: #{TEAMCITY_TOKEN}
  YAML
  File.chmod(0o600, path)
end

write_config(CONFIG_PATH, broker: broker, teamcity: teamcity, log_dir: LOG_DIR)
CONFIG_FINGERPRINT = [File.read(CONFIG_PATH), File.stat(CONFIG_PATH).mode]

CONNECTOR_STDERR = File.join(WORK, 'connector.stderr')

def start_connector
  env = {
    'FAKE_P4_ARGV_LOG' => ARGV_LOG,
    'PATH' => ENV.fetch('PATH', '/usr/bin:/bin'),
    'HOME' => ENV.fetch('HOME', '/tmp'),
    # Deliberately present and deliberately ignored: the config loader has no
    # environment fallback for any credential.
    'BUTTERSTACK_CONNECTOR_TOKEN' => 'bsc_evil_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
    'P4PASSWD' => 'env-ticket-should-not-be-used'
  }
  pid = Process.spawn(env, CONNECTOR_BIN, '-config', CONFIG_PATH, unsetenv_others: true,
                      out: [CONNECTOR_STDERR, 'a'], err: [CONNECTOR_STDERR, 'a'])
  at_exit { begin; Process.kill('KILL', pid); rescue StandardError; nil; end }
  pid
end

def stop_connector(pid, signal: 'TERM')
  Process.kill(signal, pid)
  Process.waitpid(pid)
rescue Errno::ESRCH, Errno::ECHILD
  nil
end

def audit_lines
  Dir[File.join(LOG_DIR, 'audit-*.log')].flat_map { |f| File.readlines(f) }
       .filter_map { |l| JSON.parse(l) rescue nil }
end

def audit_for(command_id)
  audit_lines.find { |e| e['command_id'] == command_id }
end

# ===========================================================================
# Phase 0: the connector connects and the compiled verbs round-trip.
# ===========================================================================
PID = start_connector
session = broker.wait_for_session(20)
abort "drills: the connector never connected. stderr:\n#{File.read(CONNECTOR_STDERR)}" unless session

LATENCIES = {}

drill('P0', 'compiled verbs round-trip end to end') do |a|
  hello = session.hello
  a.call('hello announces the compiled vocabulary',
         hello['capabilities'].sort == %w[p4.changes p4.describe sys.capabilities sys.ping sys.version
                                          teamcity.build.get teamcity.server.info],
         hello['capabilities'].sort.join(','))
  a.call('hello announces no mutating or content verb',
         hello['capabilities'].none? { |v| v =~ /trigger|queue|contents|log\.tail/ })

  %w[sys.ping sys.version sys.capabilities].each do |verb|
    t0 = Time.now
    res = broker.call(verb)
    LATENCIES[verb] = Time.now - t0
    a.call("#{verb} returns ok", res['status'] == 'ok', res['reason'])
  end

  t0 = Time.now
  info = broker.call('teamcity.server.info')
  LATENCIES['teamcity.server.info'] = Time.now - t0
  a.call('teamcity.server.info returns ok', info['status'] == 'ok', info['reason'])
  a.call('teamcity.server.info projects only the declared fields',
         info.dig('body', 'version').to_s.start_with?('2025.03') &&
           info['body'].keys.sort == %w[buildNumber version versionMajor versionMinor webUrl],
         info['body']&.keys&.join(','))

  t0 = Time.now
  build = broker.call('teamcity.build.get', { 'build_id' => 9001 })
  LATENCIES['teamcity.build.get'] = Time.now - t0
  a.call('teamcity.build.get returns ok', build['status'] == 'ok', build['reason'])
  a.call('teamcity.build.get returns the build we seeded',
         build.dig('body', 'number') == '512' && build.dig('body', 'buildTypeId') == 'Uat_Build')

  t0 = Time.now
  desc = broker.call('p4.describe', { 'change' => 41_337 })
  LATENCIES['p4.describe'] = Time.now - t0
  a.call('p4.describe returns ok', desc['status'] == 'ok', desc['reason'])
  a.call('p4.describe returns the file list and no diff',
         desc.dig('body', 'file_count') == 2 && !desc['body'].key?('diff'))

  t0 = Time.now
  changes = broker.call('p4.changes', { 'path' => '//depot/game/...' })
  LATENCIES['p4.changes'] = Time.now - t0
  a.call('p4.changes returns ok', changes['status'] == 'ok', changes['reason'])

  a.call('every verb round-trips in under 2s',
         LATENCIES.values.all? { |v| v < 2.0 },
         LATENCIES.map { |k, v| format('%s=%.0fms', k, v * 1000) }.join(' '))

  # Local credential custody, positively: the TeamCity stub only answers a
  # request bearing the token that lives in connector.yml.
  a.call('the TeamCity token used on the LAN came from connector.yml',
         teamcity.requests.any? && teamcity.requests.all? { |r| r[:authorization] == "Bearer #{TEAMCITY_TOKEN}" })
end

# ===========================================================================
# D1: an out-of-vocabulary verb is denied.
# ===========================================================================
drill('D1', 'out-of-vocabulary verb is denied') do |a|
  %w[sys.exec p4.print teamcity.build.delete jenkins.node.exec].each do |verb|
    id = session.issue(verb, {}, deadline_ms: 5000, max_bytes: 0)
    res = session.await(id, 10)
    a.call("#{verb} is denied", res && res['status'] == 'denied', res && res['reason'])
    a.call("#{verb} is denied as unknown_verb", res && res['reason'] == 'unknown_verb')
    entry = audit_for(id)
    a.call("#{verb} wrote a local audit line", entry && entry['status'] == 'denied')
  end

  # A verb name the schema reserves but this build does not compile in is
  # refused the same way. This is what makes "a new verb requires a connector
  # version with that verb compiled in" true rather than aspirational.
  %w[jenkins.build.trigger teamcity.build.queue p4.file_contents horde.server.info].each do |verb|
    id = session.issue(verb, {}, deadline_ms: 5000, max_bytes: 0)
    res = session.await(id, 10)
    a.call("reserved #{verb} is denied as verb_not_compiled",
           res && res['status'] == 'denied' && res['reason'] == 'verb_not_compiled',
           res && res['reason'])
  end
end

# ===========================================================================
# D2: an in-vocabulary verb with an out-of-scope argument is denied.
#     This is the drill that actually tests survival condition 2; D1 alone only
#     tests the dispatcher.
# ===========================================================================
drill('D2', 'in-vocabulary verb with an out-of-scope argument is denied') do |a|
  cases = [
    ['p4.changes',         { 'path' => '//...' },                          'out_of_scope_path',
     'the whole depot, which the P4 user may well be able to read'],
    ['p4.changes',         { 'path' => '//depot/...' },                    'out_of_scope_path',
     'a wildcard one level above the scoped prefix'],
    ['p4.changes',         { 'path' => '//other/secrets/...' },            'out_of_scope_path',
     'a different depot entirely'],
    ['p4.changes',         { 'path' => '//depot/game/../../other/...' },   'argument_pattern',
     'traversal out of the scoped prefix'],
    ['p4.describe',        { 'change' => '41337' },                        'argument_type',
     'a quoted integer'],
    ['p4.describe',        { 'change' => 41_337, 'include_diff' => true }, 'content_verb_disabled',
     'a content toggle the caller does not get to set'],
    ['p4.describe',        { 'change' => 41_337, 'params' => { 'X' => '$(id)' } }, 'unknown_argument',
     'a smuggled parameter bag'],
    ['teamcity.build.get', { 'build_id' => 9001, 'properties' => { 'env.X' => '1' } }, 'unknown_argument',
     'TeamCity properties, which build steps consume'],
    ['teamcity.build.get', { 'build_id' => 0 },                            'argument_range', nil],
    ['teamcity.build.get', { 'build_id' => '9001 OR 1=1' },                'argument_type', nil]
  ]

  cases.each do |verb, args, reason, note|
    id = session.issue(verb, args, deadline_ms: 5000, max_bytes: 0)
    res = session.await(id, 10)
    a.call("#{verb} #{JSON.generate(args)} is denied", res && res['status'] == 'denied', res && res['reason'])
    a.call("  ... as #{reason}", res && res['reason'] == reason, note)
    entry = audit_for(id)
    a.call('  ... with a local audit line', entry && entry['status'] == 'denied' && entry['reason'] == reason)
  end

  # The denial happened before any tool call: the fake p4 never saw these.
  argv = File.exist?(ARGV_LOG) ? File.readlines(ARGV_LOG).map { |l| JSON.parse(l) } : []
  a.call('no denied p4 verb reached the p4 binary',
         argv.none? { |v| v.join(' ').include?('//other') || v.join(' ') =~ %r{//\.\.\.} },
         "#{argv.size} p4 invocations recorded")

  # And the no-shell property: a path carrying shell metacharacters, inside the
  # scope, arrives at p4 as exactly one argv element with its bytes unchanged.
  metachar_path = '//depot/game/$(touch ' + File.join(WORK, 'pwned') + ')/...'
  id = session.issue('p4.changes', { 'path' => metachar_path }, deadline_ms: 5000, max_bytes: 0)
  res = session.await(id, 10)
  a.call('an in-scope path with shell metacharacters is executed, not interpreted',
         res && res['status'] == 'ok', res && res['reason'])
  a.call('no shell ran: the side-effect file does not exist', !File.exist?(File.join(WORK, 'pwned')))
  argv = File.readlines(ARGV_LOG).map { |l| JSON.parse(l) }
  a.call('the path arrived as one literal argv element',
         argv.any? { |v| v.include?(metachar_path) },
         'argv array invocation, no shell interpretation')
end

# ===========================================================================
# D3: a query-string token on the /connect upgrade is refused before any
#     session state is allocated. (Devin section 4.3 drill (g).)
# ===========================================================================
drill('D3', 'query-string token on /connect is rejected before session state') do |a|
  before = broker.sessions.size

  # The connector itself refuses to start with such an endpoint: the client half
  # of the rule, so a copy-pasted URL cannot leak a token into a proxy log.
  bad_cfg = File.join(WORK, 'connector-querystring.yml')
  File.write(bad_cfg, File.read(CONFIG_PATH).sub(broker.endpoint, "#{broker.endpoint}?token=#{TOKEN}"))
  File.chmod(0o600, bad_cfg)
  _out, err, status = Open3.capture3(CONNECTOR_BIN, '-config', bad_cfg)
  a.call('the connector refuses an endpoint carrying a query string',
         !status.success? && err.include?('query string'), err.strip.lines.first&.strip)

  # And the broker half: a raw client that puts the token in the URL and sends
  # no Authorization header is refused with an HTTP status, before the upgrade.
  require 'socket'
  raw = TCPSocket.new('127.0.0.1', broker.port)
  ctx = OpenSSL::SSL::SSLContext.new
  ctx.cert_store = OpenSSL::X509::Store.new.tap { |s| s.add_file(broker.ca_pem_path) }
  ctx.verify_mode = OpenSSL::SSL::VERIFY_PEER
  ssl = OpenSSL::SSL::SSLSocket.new(raw, ctx)
  ssl.hostname = 'localhost'
  ssl.connect
  ssl.write("GET /connect?token=#{TOKEN} HTTP/1.1\r\n" \
            "Host: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" \
            "Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\nSec-WebSocket-Version: 13\r\n\r\n")
  response = begin
    ssl.readpartial(1024)
  rescue StandardError
    ''
  end
  ssl.close rescue nil

  a.call('the broker answers an HTTP refusal, never a 101',
         response.start_with?('HTTP/1.1 400'), response.lines.first&.strip)
  a.call('the refusal was recorded as a query-string token',
         broker.refusals.any? { |r| r[:reason] == 'query_string_token' })
  a.call('no session state was allocated', broker.sessions.size == before)

  # A missing and a wrong bearer token are refused the same way.
  %w[missing wrong].each do |kind|
    raw2 = TCPSocket.new('127.0.0.1', broker.port)
    ssl2 = OpenSSL::SSL::SSLSocket.new(raw2, ctx)
    ssl2.hostname = 'localhost'
    ssl2.connect
    auth = kind == 'wrong' ? "Authorization: Bearer bsc_intg7f3a_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ\r\n" : ''
    ssl2.write("GET /connect HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n" \
               "Connection: Upgrade\r\nSec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\n" \
               "Sec-WebSocket-Version: 13\r\n#{auth}\r\n")
    resp2 = begin
      ssl2.readpartial(1024)
    rescue StandardError
      ''
    end
    ssl2.close rescue nil
    a.call("a #{kind} bearer token is refused with 401", resp2.start_with?('HTTP/1.1 401'), resp2.lines.first&.strip)
  end
  a.call('no session state was allocated for any refusal', broker.sessions.size == before)
end

# ===========================================================================
# (f) broker-side half: a result frame carrying another session's command id is
#     discarded rather than dispatched by id alone.
# ===========================================================================
drill('F-partial', 'a result frame for another session\'s command id is discarded') do |a|
  before = broker.discarded_results.size

  # Send a command whose id the broker never records as outstanding. The
  # connector answers it, and the answer arrives as a result this session never
  # asked for -- the same shape as a result frame carrying another session's
  # command id.
  ghost = session.issue_unregistered('sys.ping', {}, deadline_ms: 5000, max_bytes: 0)
  a.call('the session does not know the ghost command id', !session.known_command?(ghost))

  deadline = Time.now + 10
  sleep 0.1 while broker.discarded_results.size == before && Time.now < deadline
  discarded = broker.discarded_results.last
  a.call('the unmatched result was discarded, not dispatched by id alone',
         broker.discarded_results.size > before && discarded && discarded[:command_id] == ghost,
         discarded.inspect)
  a.call('matching is per session, not global',
         session.deliver({ 'type' => 'result', 'id' => SecureRandom.uuid, 'status' => 'ok' }) == false)

  # And the session still works: a discarded frame is not a fatal condition.
  a.call('the session survives an unmatched result', broker.call('sys.ping')['status'] == 'ok')
end

# ===========================================================================
# R1: the studio's network drops. The connector reconnects with no operator
#     action on the studio side.
# ===========================================================================
drill('R1', 'network drop: the connector reconnects unaided') do |a|
  a.call('a session is up before the drop', broker.online?)
  t0 = Time.now
  broker.partition(3)
  a.call('the session went down with the network', broker.wait_for_offline(10))
  sleep 3.2
  session = broker.wait_for_session(45)
  a.call('the connector reconnected without operator action', !session.nil?,
         format('%.1fs to recover', Time.now - t0))
  a.call('the reconnected session works', session && broker.call('sys.ping', session: session)['status'] == 'ok')
  a.call('the audit log recorded the reconnect schedule',
         audit_lines.any? { |e| e['event'] == 'reconnect_scheduled' })
end

# ===========================================================================
# R3: our side is stopped and restarted. (Run before R2 so the connector is
#     still alive.) The connector backs off, then reconnects.
# ===========================================================================
drill('R3', 'broker stopped and restarted: backoff then reconnect') do |a|
  broker.stop
  a.call('the session went down when the broker stopped', broker.wait_for_offline(10))
  sleep 2
  broker.start
  s = broker.wait_for_session(60)
  a.call('the connector reconnected to the restarted broker', !s.nil?)
  a.call('the reconnected session serves verbs', s && broker.call('sys.ping', session: s)['status'] == 'ok')

  backoffs = audit_lines.select { |e| e['event'] == 'reconnect_scheduled' }
  a.call('backoff was scheduled and logged, never an escalation',
         backoffs.size >= 2 && backoffs.none? { |e| e['status'] == 'error' },
         "#{backoffs.size} reconnect_scheduled lines")
end

# ===========================================================================
# R4: the token is revoked on our side. The socket closes within one heartbeat,
#     and the studio's own credentials are untouched.
# ===========================================================================
drill('R4', 'token revoked: socket closes within one heartbeat, studio credentials untouched') do |a|
  a.call('a session is up before the revoke', broker.wait_for_session(20))
  t0 = Time.now
  closed = broker.revoke(TOKEN)
  a.call('the broker closed the live session', closed.positive?)
  a.call('the socket was gone within one heartbeat',
         broker.wait_for_offline(MockBroker::HEARTBEAT_INTERVAL + 2),
         format('%.2fs', Time.now - t0))

  # Reconnect attempts are now refused, and refused the same way every time.
  sleep 4
  a.call('reconnects with the revoked token are refused',
         broker.refusals.any? { |r| r[:reason] == 'revoked_token' },
         "#{broker.refusals.count { |r| r[:reason] == 'revoked_token' }} refusals")
  a.call('no session came back up', !broker.online?)

  # The studio side is untouched: connector.yml is byte-identical, still 0600,
  # and no tool credential ever crossed the socket.
  a.call('connector.yml is unchanged and still 0600',
         [File.read(CONFIG_PATH), File.stat(CONFIG_PATH).mode] == CONFIG_FINGERPRINT)
  wire = broker.received_frames.join("\n")
  a.call('the TeamCity token never crossed the socket', !wire.include?(TEAMCITY_TOKEN))
  a.call('the P4 ticket never crossed the socket', !wire.include?(P4_TICKET))
  a.call('the connector token never appeared in a frame body', !wire.include?(TOKEN))
  a.call('no LAN host, port, or URL appeared in an inbound frame',
         !wire.include?(teamcity.base_url) && !wire.include?('ssl:p4.lan:1666'))
end

# ===========================================================================
# R2: the connector is stopped. We go offline and every verb-dependent feature
#     renders the degraded state instead of raising.
# ===========================================================================
drill('R2', 'connector stopped: we flip to offline and degrade, not error') do |a|
  # Re-issue a token so there is a live session to stop.
  token2 = new_token
  broker.register_token(token2, integration_id: 'intg7f3a')
  cfg2 = File.join(WORK, 'connector-2.yml')
  File.write(cfg2, File.read(CONFIG_PATH).sub(TOKEN, token2))
  File.chmod(0o600, cfg2)
  env2 = { 'FAKE_P4_ARGV_LOG' => ARGV_LOG, 'PATH' => ENV.fetch('PATH', '/usr/bin:/bin'),
           'HOME' => ENV.fetch('HOME', '/tmp') }
  pid2 = Process.spawn(env2, CONNECTOR_BIN, '-config', cfg2, unsetenv_others: true,
                       out: [CONNECTOR_STDERR, 'a'], err: [CONNECTOR_STDERR, 'a'])
  s = broker.wait_for_session(30)
  a.call('the second connector connected', !s.nil?)
  a.call('it serves verbs while up', s && broker.call('sys.ping', session: s)['status'] == 'ok')

  t0 = Time.now
  stop_connector(pid2, signal: 'TERM')
  a.call('stopping the connector takes us offline',
         broker.wait_for_offline(10), format('%.2fs', Time.now - t0))

  # This is the kill-switch UX: a verb-dependent feature records "needs
  # connector" and moves on rather than raising.
  degraded = broker.degraded_call('teamcity.build.get')
  a.call('a verb-dependent feature renders the degraded state, not an error',
         degraded['status'] == 'needs_connector', degraded.inspect)
  a.call('the connector shut down cleanly and logged it',
         audit_lines.any? { |e| e['event'] == 'shutdown' })
end

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------
stop_connector(PID)
broker.stop
teamcity.stop

puts
puts '=' * 78
puts 'butterstack-connector drills (issue #1575 group 1) -- design note section 4.3'
puts '=' * 78
RESULTS.each do |r|
  puts format('%-10s %-4s %-62s %s', r.name, r.ok ? 'ok' : 'FAIL', r.title, r.evidence)
end
puts '-' * 78
puts 'Round-trip latency (local loopback; the design targets under 2s over a home connection):'
LATENCIES.sort.each { |verb, secs| puts format('  %-24s %6.1f ms', verb, secs * 1000) }
puts '-' * 78
failed = RESULTS.reject(&:ok)
if failed.empty?
  puts "All #{RESULTS.size} drills passed."
else
  puts "#{failed.size} of #{RESULTS.size} drills FAILED:"
  failed.each do |r|
    puts "  #{r.name} #{r.title}"
    r.detail.select { |d| d.start_with?('FAIL') }.each { |d| puts "      #{d}" }
  end
end
puts '=' * 78
exit(failed.empty? ? 0 : 1)
