# frozen_string_literal: true

# A tiny stand-in for an on-prem TeamCity server, on the studio's LAN side of
# the connector.
#
# It exists to prove one thing the drills care about: the Bearer token the
# connector presents to TeamCity comes from connector.yml and never crosses the
# broker socket. The stub refuses any request without the exact token the config
# file holds, so a connector that had somehow lost local custody would fail the
# round-trip drills rather than quietly passing them.
require 'json'
require 'socket'

class TeamCityStub
  attr_reader :port, :requests

  def initialize(token:, logger: nil)
    @token = token
    @logger = logger
    @requests = []
    @mutex = Mutex.new
    @builds = {}
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
    @server = TCPServer.new('127.0.0.1', 0)
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
    _method, target, = lines.shift.split(' ')
    headers = lines.each_with_object({}) do |l, h|
      k, v = l.split(':', 2)
      h[k.to_s.strip.downcase] = v.to_s.strip if k && v
    end
    path, query = target.split('?', 2)
    @mutex.synchronize { @requests << { path: path, query: query, authorization: headers['authorization'] } }

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

  def respond(sock, status, body)
    json = JSON.generate(body)
    sock.write("HTTP/1.1 #{status} #{status == 200 ? 'OK' : 'Error'}\r\n" \
               "Content-Type: application/json\r\n" \
               "Content-Length: #{json.bytesize}\r\n" \
               "Connection: close\r\n\r\n#{json}")
  end
end
