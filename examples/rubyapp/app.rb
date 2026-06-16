require 'webrick'

port = (ENV['PORT'] || 8080).to_i

server = WEBrick::HTTPServer.new(
  Port:        port,
  BindAddress: '0.0.0.0',
  Logger:      WEBrick::Log.new($stderr, WEBrick::BasicLog::INFO),
  AccessLog:   [[
    $stderr,
    WEBrick::AccessLog::COMBINED_LOG_FORMAT
  ]]
)

server.mount_proc '/' do |_req, res|
  res['Content-Type'] = 'text/plain'
  res.body = "Hello from Ruby on a unikernel!\n"
end

trap('INT') { server.shutdown }
server.start
