#!/usr/bin/env ruby
# Execute workflow metadata, replay, failure and capture steps with a synthetic Go stub.
require 'yaml'
require 'tmpdir'
require 'fileutils'
require 'open3'
workflow = YAML.load_file(File.join(__dir__, '../.github/workflows/property-discovery.yml'))
Dir.mktmpdir('property-capture-') do |root|
  bin = File.join(root, 'bin')
  FileUtils.mkdir_p(bin)
  stub = File.join(bin, 'go')
  File.write(stub, <<~'SH')
    #!/usr/bin/env bash
    if [[ "$1" == env ]]; then echo go1.26.5; exit 0; fi
    printf '%s\n' "$@" > "$STUB_ARGS"
    mkdir -p internal/publish/testdata/rapid
    printf synthetic > internal/publish/testdata/rapid/replay.fail
    exit 23
  SH
  FileUtils.chmod(0755, stub)
  %w[filesystem-linux filesystem-macos].each do |job_name|
    job = workflow.fetch('jobs').fetch(job_name)
    steps = job.fetch('steps')
    env = job.fetch('env').transform_values { |v| v.to_s.include?('${{') ? '7' : v.to_s }
    env.merge!('PATH'=>"#{bin}:#{ENV.fetch('PATH')}", 'RUNNER_TEMP'=>File.join(root, job_name), 'STUB_ARGS'=>File.join(root, 'args'), 'GITHUB_SHA'=>'a'*40, 'GITHUB_EVENT_NAME'=>'workflow_dispatch', 'RUNNER_ARCH'=>'ARM64')
    work = File.join(root, "work-#{job_name}")
    FileUtils.mkdir_p(work)
    execute = lambda do |script|
      out, err, status = Open3.capture3(env, 'bash', '-euo', 'pipefail', '-c', script, chdir: work)
      [status.exitstatus, out, err]
    end
    metadata = steps.find { |s| s['name'] == 'Write safe metadata before search' }.fetch('run')
    raise 'metadata failed' unless execute.call(metadata)[0] == 0
    manifest = File.read(File.join(env['RUNNER_TEMP'], 'property-artifacts/manifest.txt'))
    command = manifest.lines.find { |l| l.start_with?('command=') }.delete_prefix('command=').strip
    raise 'replay lost failure exit' unless execute.call(command)[0] == 23
    args = File.readlines(env['STUB_ARGS'], chomp: true)
    raise 'replay regex split into pipelines' unless args[args.index('-run') + 1] == 'Generated|History|ReloadAuthority|ReaderAtRename'
    search = steps.find { |s| s['id'] == 'histories' }.fetch('run')
    raise 'search lost failure exit' unless execute.call(search)[0] == 23
    capture = steps.find { |s| s['name'] == 'Capture allowlisted failure inputs' }
    raise 'capture not failure conditional' unless capture['if'] == 'failure()'
    raise 'capture failed' unless execute.call(capture.fetch('run'))[0] == 0
    raise 'synthetic failure not captured' unless Dir.glob(File.join(env['RUNNER_TEMP'], 'property-artifacts/failures/**/*.fail')).length == 1
  end
end
puts 'property replay/capture fixtures passed'
