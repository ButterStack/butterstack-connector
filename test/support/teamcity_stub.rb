# frozen_string_literal: true

# A tiny stand-in for an on-prem TeamCity server, on the studio's LAN side of
# the connector.
#
# It exists to prove one thing the drills care about: the Bearer token the
# connector presents to TeamCity comes from connector.yml and never crosses the
# broker socket. The stub refuses any request without the exact token the config
# file holds, so a connector that had somehow lost local custody would fail the
# round-trip drills rather than quietly passing them.
#
# UAT addition (connector.spec.js, issue #1574/#1575): this file is also run
# standalone as a container in docker-compose.uat-connector.yml, modelling the
# studio's real on-prem TeamCity. Two extra jobs on top of the original
# in-process API, both admin-only and unauthenticated (LAN-internal, drill/UAT
# use only -- never anything a real TeamCity exposes):
#
#   POST /uat/builds          -- seed a build from JSON, so the standalone
#                                 process can be seeded over HTTP instead of a
#                                 constructor call.
#   POST /uat/webhooks/fire   -- fire an OUTBOUND webhook at a ButterStack
#                                 endpoint, in either of the two shapes a real
#                                 TeamCity delivery can take: its own built-in
#                                 webhook envelope ("native"), or the flat
#                                 payload a curl build step produces
#                                 ("curl_step"). This proves the same intake
#                                 endpoint that accepts a Jenkins/generic-CI
#                                 payload also accepts what TeamCity actually
#                                 sends, for both of the Tier 1 wiring options.
require 'json'
require 'socket'
require 'net/http'
require 'uri'

class TeamCityStub
  attr_reader :port, :requests

  def initialize(token:, logger: nil, bind: '127.0.0.1', port: 0)
    @token = token
    @logger = logger
    @requests = []
    @mutex = Mutex.new
    @builds = {}
    @bind = bind
    @port = port
  end

  def add_build(id, build_type_id:, number:, status: 'SUCCESS', state: 'finished', revision: nil)
    @mutex.synchronize do
      @builds[id.to_i] = {
        'id' => id.to_i, 'buildTypeId' => build_type_id, 'number' => number,
        'status' => status, 'state' => state, 'statusText' => 'Tests passed',
        'branchName' => 'refs/heads/main',
        'webUrl' => "http://teamcity.invalid/viewLog.html?buildId=#{id}",
        'queuedDate' => '20260830T100000+0000',
        'startDate' => '20260830T100005+0000',
        'finishDate' => '20260830T100205+0000',
        'revisions' => { 'revision' => [{ 'version' => revision || '41337', 'vcsBranchName' => '//depot/game/main' }] }
      }
    end
  end

  def start
    @server = TCPServer.new(@bind, @port || 0)
    @port = @server.addr[1]
    @running = true
    @thread = Thread.new { accept_loop }
    self
  end

  def stop
    @running = false
    begin
      @server&.close
    rescue StandardError
      nil
    end
    @thread&.kill
  end

  def base_url
    "http://127.0.0.1:#{@port}"
  end

  private

  def accept_loop
    while @running
      begin
        sock = @server.accept
      rescue StandardError
        break
      end
      Thread.new(sock) { |s| serve(s) }
    end
  end

  def serve(sock)
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
    path, query = target.split('?', 2)
    @mutex.synchronize { @requests << { path: path, query: query, authorization: headers['authorization'] } }

    # UAT/drill admin endpoints. Unauthenticated on purpose: this stub only
    # ever runs on the studio_lan network (no host port published), which is
    # exactly the boundary the connector.spec.js isolation phase (100) proves.
    case [method, path]
    when ['POST', '/uat/builds']
      return handle_uat_add_build(sock, body)
    when ['POST', '/uat/webhooks/fire']
      return handle_uat_fire_webhook(sock, body)
    end

    return respond(sock, 401, { 'error' => 'unauthorized' }) unless headers['authorization'] == "Bearer #{@token}"

    case path
    when '/app/rest/server'
      respond(sock, 200, {
                'version' => '2025.03 (build 189123)', 'versionMajor' => 2025, 'versionMinor' => 3,
                'buildNumber' => '189123', 'webUrl' => 'http://teamcity.invalid'
              })
    when %r{\A/app/rest/builds/id:(\d+)\z}
      build = @mutex.synchronize { @builds[Regexp.last_match(1).to_i] }
      build ? respond(sock, 200, build) : respond(sock, 404, { 'error' => 'not found' })
    else
      respond(sock, 404, { 'error' => 'not found' })
    end
  rescue StandardError => e
    @logger&.call("[teamcity-stub] #{e.class}: #{e.message}")
  ensure
    begin
      sock.close
    rescue StandardError
      nil
    end
  end

  def handle_uat_add_build(sock, body)
    params = JSON.parse(body)
    id = params.fetch('id')
    add_build(id,
              build_type_id: params.fetch('build_type_id'),
              number: params.fetch('number'),
              status: params['status'] || 'SUCCESS',
              state: params['state'] || 'finished',
              revision: params['revision'])
    respond(sock, 200, { 'ok' => true, 'id' => id.to_i })
  rescue KeyError, JSON::ParserError => e
    respond(sock, 400, { 'error' => e.message })
  end

  # handle_uat_fire_webhook is the UAT stand-in for what a real TeamCity
  # delivers on "Build finished": either its own built-in webhook envelope
  # (teamcity.internal.webhooks.*, mode "native") or the flat payload a curl
  # build step composes (mode "curl_step") with the credential in
  # X-Webhook-Token, matching the recipe in the #1574 Tier 1 wiring doc.
  #
  # A real TeamCity 2026.1.3 server (captured on Ryan's LAN with a request
  # logger; see test/fixtures/files/teamcity/delivery_headers.json) sends its
  # built-in webhook with a standard `Authorization: Basic
  # base64(username:password)` header built from
  # teamcity.internal.webhooks.username/.password when `.password` is a
  # plain-typed parameter -- never php-auth-user/php-auth-pw. With a
  # Password-typed `.password`, no Authorization header is sent at all. Mode
  # "native" therefore defaults to Basic auth (the real channel) and only
  # falls back to the non-standard php-auth-user/php-auth-pw pair when
  # `auth: "php-auth"` is passed explicitly -- kept because
  # WebhookTokenSources still accepts it as a secondary credential channel
  # (#935). Passing neither username nor password models the Password-typed
  # case: no Authorization header at all.
  #
  # This request is OUTBOUND from the studio's TeamCity to ButterStack's
  # webhook intake -- exactly the direction studio_lan permits.
  def handle_uat_fire_webhook(sock, body)
    params = JSON.parse(body)
    mode = params.fetch('mode')
    build_id = params.fetch('build_id')
    url = URI.parse(params.fetch('url'))
    build = @mutex.synchronize { @builds[build_id.to_i] }
    raise "no such seeded build #{build_id}" unless build

    req = Net::HTTP::Post.new(url)
    req['Content-Type'] = 'application/json'
    # If the receiving app enforces Host-header allowlisting (production/
    # staging config.hosts), the outbound hop here goes through studio-egress,
    # whose compose hostname is not on that list. Pin the Host header to one
    # that is, so the drill exercises intake auth rather than host rejection.
    req['Host'] = params['host_header'] || 'localhost'

    case mode
    when 'native'
      # Real TeamCity identifies itself with this exact User-Agent on every
      # request, regardless of auth channel.
      req['User-Agent'] = 'TeamCity Server 2026.1.3 (build 222742)'

      # Credentials are optional on purpose: omitting username/password lets
      # the connector.spec.js "missing credential" drill (phase 420) fire a
      # request with no Authorization channel at all, distinct from a
      # present-but-wrong one.
      if params['auth'] == 'php-auth'
        req['php-auth-user'] = params['username'] if params['username']
        req['php-auth-pw'] = params['password'] if params['password']
      elsif params['username'] || params['password']
        creds = "#{params['username']}:#{params['password']}"
        req['Authorization'] = "Basic #{[creds].pack('m0')}"
      end

      req.body = JSON.generate({ 'eventType' => 'BUILD_FINISHED', 'payload' => native_payload_for(build) })
    when 'curl_step'
      req['X-Webhook-Token'] = params['token'] if params['token']
      flat = {
        'job_name' => build['buildTypeId'],
        'build_number' => build['number'],
        'status' => (build['status'] == 'SUCCESS') ? 'success' : 'failure',
        'build_url' => build['webUrl'],
        'changelist' => build.dig('revisions', 'revision', 0, 'version')
      }
      flat['logs_tail'] = params['logs_tail'] if params['logs_tail']
      req.body = JSON.generate(flat)
    else
      raise "unknown mode #{mode.inspect}"
    end

    http = Net::HTTP.new(url.host, url.port)
    http.open_timeout = 10
    http.read_timeout = 15
    response = http.request(req)
    respond(sock, 200, { 'upstream_status' => response.code.to_i, 'upstream_body' => response.body.to_s })
  rescue StandardError => e
    respond(sock, 502, { 'error' => "#{e.class}: #{e.message}" })
  end

  # native_payload_for builds the "native" envelope's payload in the shape of
  # a real TeamCity REST Build resource -- the same keys captured in
  # test/fixtures/files/teamcity/build_finished_success.json (id,
  # buildTypeId, number, status, state, href, webUrl, statusText,
  # buildType{id,name,projectName,projectId,href,webUrl}, queuedDate/
  # startDate/finishDate, triggered, changes, revisions, agent, artifacts,
  # properties) -- rather than the reconstructed minimal object this stub
  # used to hand back. It's inlined here (not read from that fixture at
  # runtime) because the teamcity-stub container only mounts connector/test,
  # not the repo-root test/fixtures tree.
  #
  # The seeded build's own id/buildTypeId/number/status/state/revision are
  # threaded through verbatim, including `revisions`, so the curl_step and
  # native paths carry the same version and the Perforce path still has one
  # to chase.
  def native_payload_for(build)
    build_type_id = build['buildTypeId']
    {
      'id' => build['id'],
      'buildTypeId' => build_type_id,
      'number' => build['number'],
      'status' => build['status'],
      'state' => build['state'],
      'href' => "/app/rest/builds/id:#{build['id']}",
      'webUrl' => build['webUrl'],
      'statusText' => build['statusText'],
      'buildType' => {
        'id' => build_type_id,
        'name' => build_type_id.to_s.tr('_', ' '),
        'projectName' => 'ButterStack UAT Fixture',
        'projectId' => 'ButterStackUatFixture',
        'href' => "/app/rest/buildTypes/id:#{build_type_id}",
        'webUrl' => "http://teamcity.invalid/buildConfiguration/#{build_type_id}?mode=builds"
      },
      'queuedDate' => build['queuedDate'],
      'startDate' => build['startDate'],
      'finishDate' => build['finishDate'],
      'triggered' => {
        'type' => 'user',
        'date' => build['queuedDate'],
        'user' => { 'username' => 'uat', 'id' => 1, 'href' => '/app/rest/users/id:1' }
      },
      'changes' => { 'href' => "/app/rest/changes?locator=build:(id:#{build['id']})" },
      'revisions' => build['revisions'],
      'agent' => {
        'id' => 1, 'name' => 'teamcity-agent-1', 'typeId' => 1,
        'href' => '/app/rest/agents/id:1',
        'webUrl' => 'http://teamcity.invalid/agentDetails.html?id=1&agentTypeId=1&realAgentName=teamcity-agent-1'
      },
      'artifacts' => { 'count' => 0, 'href' => "/app/rest/builds/id:#{build['id']}/artifacts/children/" },
      'properties' => {
        'count' => 2,
        'property' => [
          { 'name' => 'teamcity.internal.webhooks.username', 'value' => 'teamcity', 'inherited' => true },
          { 'name' => 'teamcity.internal.webhooks.password', 'value' => '******', 'inherited' => true }
        ]
      }
    }
  end

  def respond(sock, status, body)
    json = JSON.generate(body)
    sock.write("HTTP/1.1 #{status} #{status == 200 ? 'OK' : 'Error'}\r\n" \
               "Content-Type: application/json\r\n" \
               "Content-Length: #{json.bytesize}\r\n" \
               "Connection: close\r\n\r\n#{json}")
  end
end

# Standalone entry point: `ruby teamcity_stub.rb`, for the UAT container.
# Reads TC_TOKEN (required), TC_BIND / TC_PORT (default 0.0.0.0:8111), and
# optionally seeds one build from TC_SEED_* env vars so the container is
# useful even before the test calls POST /uat/builds.
if $PROGRAM_NAME == __FILE__
  token = ENV.fetch('TC_TOKEN')
  bind = ENV['TC_BIND'] || '0.0.0.0'
  port = (ENV['TC_PORT'] || '8111').to_i

  stub = TeamCityStub.new(token: token, logger: ->(m) { warn(m) }, bind: bind, port: port)

  if ENV['TC_SEED_BUILD_ID']
    stub.add_build(ENV.fetch('TC_SEED_BUILD_ID').to_i,
                    build_type_id: ENV.fetch('TC_SEED_BUILD_TYPE_ID', 'Uat_Build'),
                    number: ENV.fetch('TC_SEED_BUILD_NUMBER', '512'),
                    revision: ENV['TC_SEED_REVISION'])
  end

  stub.start
  warn("[teamcity-stub] listening on #{bind}:#{stub.port}, token=#{token[0, 4]}...")
  Signal.trap('TERM') { exit(0) }
  Signal.trap('INT') { exit(0) }
  sleep
end
