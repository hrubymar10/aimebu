class Aimebu < Formula
  desc "IRC for you and your AI agents — message bus over MCP, HTTP/CLI, and a web UI"
  homepage "https://github.com/hrubymar10/aimebu"
  license "MIT"
  head "https://github.com/hrubymar10/aimebu.git", branch: "master"

  depends_on "go" => :build

  def install
    # `version` is "HEAD" for head-only installs; gets a real value once a
    # tagged release is published.
    ldflags = "-s -w -X main.version=#{version}"
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/aimebu"
  end

  service do
    run [opt_bin/"aimebu", "server", "serve"]
    keep_alive true
    working_dir var
    log_path var/"log/aimebu.log"
    error_log_path var/"log/aimebu.log"
    environment_variables AIMEBU_DATA: var/"lib/aimebu", AIMEBU_BIND: "127.0.0.1"
  end

  test do
    port = free_port
    pid = fork do
      ENV["AIMEBU_PORT"] = port.to_s
      ENV["AIMEBU_DATA"] = testpath.to_s
      exec bin/"aimebu", "server", "serve"
    end
    sleep 2
    output = shell_output("curl -sf http://127.0.0.1:#{port}/health")
    assert_match "ok", output
  ensure
    Process.kill("TERM", pid)
    Process.wait(pid)
  end
end
