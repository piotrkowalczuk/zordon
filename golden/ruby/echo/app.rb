# Conformance-test fixture: a Ruby service that mirrors back its execution
# context (env, cwd, argv, runtime version) as JSON so tests can assert on
# what zordon actually passed it. Same wire shape as golden/go/echo, plus
# two Ruby-only fields:
#
#   bundler_version — Bundler::VERSION when running under `bundle exec`,
#                     "" otherwise; proves which bundler the shim picked.
#   bundle_path     — Bundler.settings[:path], the effective gem location
#                     after config-file precedence; proves whether zordon's
#                     BUNDLE_PATH or a checkout's .bundle/config won.
#   gem_path        — Gem.path; proves the gem search path is the pinned
#                     interpreter's, with no ~/.gem/ruby/<abi> fallback.
#
# stdlib only (socket + json), so `bundle install` on the empty Gemfile
# next to it fetches nothing.
require "json"
require "socket"

addr = ARGV[ARGV.index("-addr") + 1] rescue "127.0.0.1:0"
host, port = addr.split(":")
server = TCPServer.new(host, port.to_i)
STDOUT.sync = true
puts "up #{addr}"

bundler_version = defined?(Bundler) ? Bundler::VERSION : ""
bundle_path = defined?(Bundler) ? Bundler.settings[:path].to_s : ""

loop do
  conn = server.accept
  conn.gets
  body = JSON.generate(
    "env" => ENV.to_h,
    "cwd" => Dir.pwd,
    "argv" => [$PROGRAM_NAME] + ARGV,
    "runtime_version" => RUBY_VERSION,
    "bundler_version" => bundler_version,
    "bundle_path" => bundle_path,
    "gem_path" => Gem.path,
  )
  conn.print "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: #{body.bytesize}\r\nConnection: close\r\n\r\n#{body}"
  conn.close
end
