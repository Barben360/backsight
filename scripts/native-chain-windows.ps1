# The native chain on Windows (decision 0080, M7 block 2): the native
# runtime proven end to end on GitHub's windows-latest, on every pull
# request, and on the author's VM as a standard user. One install from a
# local release signed by a throwaway key, set up from nothing with
# Hindsight's mock LLM, in its own BACKSIGHT_HOME. Its steps, in order,
# each a named check that fails the run with its name:
#
#   build             the testkey + testhooks binary, its second-lock twin, the signed release
#   install           install.ps1 from BACKSIGHT_RELEASE_DIR, trusting the throwaway key
#   setup             setup --runtime native (mock model, project <PROJECT>): healthy, the task
#                     registered, doctor without error; which start path came up (the task in
#                     the console session, or the detached supervisor) is recorded, never failed
#   dlls              every VC++ runtime DLL the server's processes loaded is an app-local copy
#                     under the native root, none from System32 (the runner may have them there)
#   store             notes stored, then found by a search
#   backup            backup create, then backup verify of its archive
#   restore           restore of that archive, a note stored after the backup: its checks pass,
#                     healthy, the backup's data exactly (that note gone)
#   upgrade-rollback  server upgrade to a second lock whose smoke fails: exit 30, rolled back to
#                     the first venv (hindsight-api.exe runs from it), healthy, the same data
#   kill-postgres     the postmaster killed: the supervisor has one back within 30 s, healthy
#   kill-supervisor   the supervisor killed: none of its postgres.exe or hindsight-api.exe is
#                     left; server start: healthy, no stray reported
#   refuse-identity   identity file changed: server start refused, nothing started, server
#                     status gives the reason; put back, healthy
#   refuse-missing    the database (native\db) moved away: server start refused, nothing started,
#                     server status gives the reason; put back, healthy
#   refuse-stray      a postgres.exe started on the data outside the supervisor's job (pg_ctl):
#                     server start reports it and stops it, healthy
#   refuse-fsync      fsync=off in supervise.json, the supervisor started by the task (server
#                     start would render it again): refused, server status names fsync, nothing
#                     started; then healthy on the data as before the refusals
#   uninstall         uninstall --delete-data: no task (schtasks /Query fails), no process, no
#                     native root, backsight's directory off the user PATH, nothing left but
#                     config\, state\ and data\backups\
#
# The user PATH the install writes is a throwaway key under HKCU
# (BACKSIGHT_TEST_USERPATH_KEY, the testhooks build): the real one is
# compared before and after, never written. Claude Code is a stand-in
# claude.cmd (Claude Code's wiring is not this chain's subject), with
# CLAUDE_CONFIG_DIR in the run's directory. The cleanup, which never
# changes the run's exit status, stops every process under the run's
# BACKSIGHT_HOME, deletes the project's tasks and their folder, the
# throwaway PATH key, %USERPROFILE%\.pg0 and %USERPROFILE%\.claude when
# this run created them, and the run's directory.
#
# Usage: powershell -NoProfile -ExecutionPolicy Bypass -File scripts/native-chain-windows.ps1 [api port]
# (default 28750 + BACKSIGHT_CI_SLOT x 10, as scripts/ports.sh; the UI
# takes port + 1111). PROJECT names the tasks' folder (default
# bs-native-chain); it never starts with backsight (never the live
# install). WORK is the parent of the run's own directory (default
# RUNNER_TEMP, else TEMP): keep it short, the venvs' paths are deep.
# KEEP=1 leaves everything. BACKSIGHT_NATIVE_HF names a directory holding
# native's models (<native>\hf): copied in before setup when it holds them,
# filled after setup when it does not. NC_DRY=1 lists the steps and stops;
# NC_LIB=1 defines the functions and runs nothing (the tests dot-source
# it).
#
# Needs Windows x86_64, Go (the builds), and uv's network (PyPI, GitHub,
# Hugging Face). Written for Windows PowerShell 5.1 and PowerShell 7.

$NCSteps = @('build', 'install', 'setup', 'dlls', 'store', 'backup', 'restore', 'upgrade-rollback',
    'kill-postgres', 'kill-supervisor', 'refuse-identity', 'refuse-missing', 'refuse-stray', 'refuse-fsync', 'uninstall')

# Hindsight's operational history: tables a recall appends to, left out
# when the data is compared (as native-chain.sh's NC_HISTORY).
$NCHistory = @('llm_requests', 'audit_log')

$NCVersion = '0.950.0'

# $NCSettle: seconds a refused start is given to start something anyway
# (a task run asked for before the refusal) before nothing started is
# checked.
$NCSettle = 5
$NCPkg = 'github.com/Barben360/backsight'

function Fail([string]$m) { throw "native-chain-windows: $m" }

function Write-NcLog([string]$m) { [Console]::Error.WriteLine($m) }

# Test-NcProject: refuses the live install's names before anything runs:
# its tasks are \backsight\..., and the cleanup deletes the project's
# folder.
function Test-NcProject([string]$p) {
    if ($p.StartsWith('backsight')) { throw "native-chain-windows: PROJECT=${p}: its tasks would be the live install's (\backsight\...), never the live install" }
    if ($p -cnotmatch '^[a-z0-9][a-z0-9-]*$') { throw "native-chain-windows: PROJECT=${p}: lowercase letters, digits and dashes only" }
}

# Get-NcSlotPort: scripts/ports.sh's slot_port: the port given, else base +
# slot x 10; refuses a slot outside 0-3, no slot on CI, and an API or UI
# port of the live installs.
function Get-NcSlotPort([int]$Base, [string]$Port) {
    $slot = $env:BACKSIGHT_CI_SLOT
    if (-not $slot -and $env:CI) { throw "BACKSIGHT_CI_SLOT is not set on this CI runner: set it (1, 2 or 3) in the workflow's env" }
    if (-not $slot) { $slot = '0' }
    if ($slot -notmatch '^[0-3]$') { throw "BACKSIGHT_CI_SLOT=${slot}: expected 1, 2 or 3 (unset off CI)" }
    if ($Port) {
        if ($Port -notmatch '^[0-9]+$') { throw "port '$Port' is not a number" }
        $p = [int]$Port
    } else { $p = $Base + 10 * [int]$slot }
    foreach ($x in @($p, ($p + 1111))) {
        if (@(8888, 9999, 18888, 19999) -contains $x) { throw "port $x belongs to a live install on this machine" }
    }
    return $p
}

# Protect-NcText: the text with the install's secret values masked (a
# failure's output goes to a CI log): every value of 8 characters or more
# in secrets.toml.
function Protect-NcText([string]$text) {
    $sec = $null
    if ($env:BACKSIGHT_HOME) { $sec = Join-Path $env:BACKSIGHT_HOME 'config\secrets.toml' }
    if (-not $sec -or -not (Test-Path -LiteralPath $sec)) { return $text }
    foreach ($l in [IO.File]::ReadAllLines($sec)) {
        if ($l -match '^[a-z_]+ *= *"([^"]+)"') {
            $v = $Matches[1]
            if ($v.Length -ge 8) { $text = $text.Replace($v, '***') }
        }
    }
    return $text
}

# ConvertTo-NcArg: one argument quoted as CommandLineToArgvW reads it.
function ConvertTo-NcArg([string]$s) {
    if ($s -ne '' -and $s -notmatch '[\s"]') { return $s }
    $b = New-Object Text.StringBuilder
    [void]$b.Append('"')
    $bs = 0
    foreach ($c in $s.ToCharArray()) {
        if ($c -eq '\') { $bs++; continue }
        if ($c -eq '"') { [void]$b.Append('\' * (2 * $bs + 1)); [void]$b.Append('"'); $bs = 0; continue }
        if ($bs -gt 0) { [void]$b.Append('\' * $bs); $bs = 0 }
        [void]$b.Append($c)
    }
    [void]$b.Append('\' * (2 * $bs))
    [void]$b.Append('"')
    return $b.ToString()
}

function Join-NcArgs([string[]]$a) { return (@($a | ForEach-Object { ConvertTo-NcArg $_ }) -join ' ') }

# Invoke-Nc runs exe with args, stdin given, extra environment pairs; its
# stdout goes to <name>.out and its stderr to <name>.err in the run's
# directory. Returns the exit code. -NoCapture redirects nothing: for a
# program that leaves a child running (pg_ctl start), which would hold
# the pipes open, and the wait with them, as long as it runs.
function Invoke-Nc {
    param([string]$Name, [string]$Exe, [string[]]$Arguments = @(), [hashtable]$Env = @{}, [string]$Stdin = '', [int]$TimeoutSec = 2400, [switch]$NoCapture)
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $psi.Arguments = Join-NcArgs $Arguments
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.WorkingDirectory = $script:work
    foreach ($k in $Env.Keys) { $psi.EnvironmentVariables[$k] = [string]$Env[$k] }
    if ($NoCapture) {
        $p = [Diagnostics.Process]::Start($psi)
        if (-not $p.WaitForExit($TimeoutSec * 1000)) {
            try { $p.Kill() } catch { }
            Fail "$Name ($Exe) still ran after $TimeoutSec s: killed"
        }
        return $p.ExitCode
    }
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [Diagnostics.Process]::Start($psi)
    $o = $p.StandardOutput.ReadToEndAsync()
    $e = $p.StandardError.ReadToEndAsync()
    if ($Stdin) { $p.StandardInput.Write($Stdin) }
    $p.StandardInput.Close()
    if (-not $p.WaitForExit($TimeoutSec * 1000)) {
        try { $p.Kill() } catch { }
        Fail "$Name ($Exe) still ran after $TimeoutSec s: killed"
    }
    $p.WaitForExit()
    [IO.File]::WriteAllText((Join-Path $script:work "$Name.out"), $o.Result)
    [IO.File]::WriteAllText((Join-Path $script:work "$Name.err"), $e.Result)
    return $p.ExitCode
}

# Bs runs the installed backsight (the PATH copy).
function Bs([string]$Name, [string[]]$Arguments, [hashtable]$Env = @{}, [string]$Stdin = '') {
    return Invoke-Nc -Name $Name -Exe $script:bin -Arguments $Arguments -Env $Env -Stdin $Stdin
}

function Read-NcText([string]$Name, [string]$Ext = 'out') {
    $f = Join-Path $script:work "$Name.$Ext"
    if (-not (Test-Path -LiteralPath $f)) { return '' }
    return [IO.File]::ReadAllText($f)
}

# Read-NcJson: <name>.out as JSON, $null when it is not.
function Read-NcJson([string]$Name) {
    try { return (Read-NcText $Name | ConvertFrom-Json) } catch { return $null }
}

# Show-Nc: each output's tail, masked, on stderr.
function Show-Nc([string[]]$Names) {
    foreach ($n in $Names) {
        foreach ($ext in 'out', 'err') {
            $t = Read-NcText $n $ext
            if (-not $t) { continue }
            $lines = @($t -split "`r?`n")
            Write-NcLog "--- $n.$ext"
            Write-NcLog (Protect-NcText (($lines | Select-Object -Last 60) -join "`n"))
        }
    }
}

# Show-NcFile: a file's tail, masked, on stderr.
function Show-NcFile([string]$Path, [int]$Lines = 60) {
    if (-not $Path -or -not (Test-Path -LiteralPath $Path)) { return }
    Write-NcLog "--- $Path"
    Write-NcLog (Protect-NcText ((Get-Content -LiteralPath $Path -Tail $Lines) -join "`n"))
}

# Test-NcUnder: path is under dir (case-blind).
function Test-NcUnder([string]$path, [string]$dir) {
    if (-not $path -or -not $dir) { return $false }
    return $path.StartsWith($dir.TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)
}

# Get-NcProcs: the processes named name (all with '*') whose image is
# under dir.
function Get-NcProcs([string]$dir, [string]$name = '*') {
    return @(Get-Process -Name $name -ErrorAction SilentlyContinue | Where-Object { Test-NcUnder $_.Path $dir })
}

# Get-NcSupervisors: the run's supervisors: the shim, current.exe, running
# `server supervise` (no backsight command of the chain runs the shim).
function Get-NcSupervisors { return @(Get-NcProcs $script:bh 'current') }

function Test-NcAlive([int]$id) { return $null -ne (Get-Process -Id $id -ErrorAction SilentlyContinue) }

function Test-NcHealthy {
    [void](Bs 'status' @('server', 'status', '--json'))
    $j = Read-NcJson 'status'
    return ($null -ne $j -and $j.ok -and $j.data.api.healthy)
}

function Wait-NcHealthy([int]$Sec, [string]$What) {
    $t = Get-Date
    while (((Get-Date) - $t).TotalSeconds -lt $Sec) {
        if (Test-NcHealthy) { return [int]((Get-Date) - $t).TotalSeconds }
        Start-Sleep -Seconds 2
    }
    Show-Nc 'status'
    Show-NcFile $script:apiLog
    Fail "the server is not healthy $What (waited $Sec s)"
}

# Wait-NcGone waits up to sec for none of the pids to run; the ones left.
function Wait-NcGone([int[]]$Ids, [int]$Sec) {
    $t = Get-Date
    while ($true) {
        $left = @($Ids | Where-Object { Test-NcAlive $_ })
        if ($left.Count -eq 0 -or ((Get-Date) - $t).TotalSeconds -ge $Sec) { return $left }
        Start-Sleep -Milliseconds 200
    }
}

# Get-NcFingerprint: the server's data fingerprint, once three reads 5 s
# apart agree (a stored memory's extraction, then its consolidation, run in
# the background).
function Get-NcFingerprint([string]$Name) {
    $last = ''; $same = 0
    for ($i = 1; $i -le 90; $i++) {
        if ((Bs $Name @('server', 'runtime', 'fingerprint', '--json')) -ne 0) { Show-Nc $Name; Fail 'server runtime fingerprint failed' }
        $fp = (Read-NcJson $Name).data
        $now = $fp | ConvertTo-Json -Depth 8 -Compress
        if ($now -eq $last) { $same++ } else { $same = 0 }
        if ($same -ge 2) { return $fp }
        $last = $now
        Start-Sleep -Seconds 5
    }
    Fail 'the fingerprint never settled'
}

# Compare-NcFingerprint: what differs between two fingerprints, one line
# each, Hindsight's history tables left out; nothing when they are equal.
function Compare-NcFingerprint($a, $b) {
    $out = @()
    $names = @(@($a.tables.PSObject.Properties.Name) + @($b.tables.PSObject.Properties.Name) | Sort-Object -Unique)
    foreach ($t in $names) {
        if ($NCHistory -contains $t) { continue }
        $x = $a.tables.$t; $y = $b.tables.$t
        $xr = if ($x) { $x.rows } else { 'none' }; $yr = if ($y) { $y.rows } else { 'none' }
        $xk = if ($x) { $x.keys_md5 } else { '' }; $yk = if ($y) { $y.keys_md5 } else { '' }
        if ($xr -ne $yr) { $out += "table ${t}: rows $xr -> $yr" } elseif ($xk -ne $yk) { $out += "table ${t}: rows $xr, keys differ" }
    }
    if ((@($a.banks) -join ',') -ne (@($b.banks) -join ',')) { $out += "banks $(@($a.banks) -join ',') -> $(@($b.banks) -join ',')" }
    $docs = @(@($a.control_documents.PSObject.Properties.Name) + @($b.control_documents.PSObject.Properties.Name) | Sort-Object -Unique)
    foreach ($d in $docs) {
        if ($a.control_documents.$d -ne $b.control_documents.$d) { $out += "control document $d" }
    }
    return $out
}

# Save-NcKeys: every table's keys now, in <name>.keys.
function Save-NcKeys([string]$Name) {
    if ((Bs $Name @('server', 'runtime', 'fingerprint', '--keys', (Join-Path $script:work "$Name.keys"), '--json')) -ne 0) {
        Show-Nc $Name; Fail 'server runtime fingerprint --keys failed'
    }
}

# Read-NcKeys: a --keys file as table -> set of its keys.
function Read-NcKeys([string]$Path) {
    $o = [IO.File]::ReadAllText($Path) | ConvertFrom-Json
    $h = @{}
    foreach ($p in $o.PSObject.Properties) {
        $set = New-Object 'System.Collections.Generic.HashSet[string]'
        foreach ($k in @($p.Value)) { [void]$set.Add([string]$k) }
        $h[$p.Name] = $set
    }
    return $h
}

# $NCEpochDoc is the restore epoch's document (retain.EpochDocument): a
# restore writes it, and a rollback writes it again.
$NCEpochDoc = 'backsight:epoch'

# Get-NcControlUnits: the memory units the control bank's links join.
function Get-NcControlUnits($keys, [string]$control) {
    $u = New-Object 'System.Collections.Generic.HashSet[string]'
    if ($keys.ContainsKey('memory_links')) {
        foreach ($k in $keys['memory_links']) {
            if ($k.EndsWith(",$control)")) {
                $f = $k.TrimStart('(').Split(',')
                [void]$u.Add("($($f[0]))"); [void]$u.Add("($($f[1]))")
            }
        }
    }
    return , $u
}

# Compare-NcKeys: the keys gone from before and added in after, one line
# each, but Hindsight's history tables and the rewrite of one control
# document, the restore epoch's ($NCEpochDoc): its own rows (the keys
# naming it), its memory unit replaced (one control unit gone and one
# added, no more; only added when the document is new, the first
# restore's), and the control bank's links that join one of those. Any other key of the control bank (its server, tombstones or
# feedback documents, their units or links) is a difference like any
# other. The epoch's changes are counted in $script:controlChanged.
function Compare-NcKeys([string]$Before, [string]$After, [string]$Control) {
    $b = Read-NcKeys $Before
    $a = Read-NcKeys $After
    $cb = Get-NcControlUnits $b $Control
    $ca = Get-NcControlUnits $a $Control
    $out = @()
    $script:controlChanged = 0
    $none = New-Object 'System.Collections.Generic.HashSet[string]'
    $units = @{ gone = @(); added = @() }
    $links = @{ gone = @(); added = @() }
    $epochNew = $false
    foreach ($t in @(@($b.Keys) + @($a.Keys) | Sort-Object -Unique)) {
        if ($NCHistory -contains $t) { continue }
        # Assigned, never an if's output: PowerShell unrolls an empty set
        # to $null there (an empty table).
        $bs = $none; if ($b.ContainsKey($t)) { $bs = $b[$t] }
        $as = $none; if ($a.ContainsKey($t)) { $as = $a[$t] }
        foreach ($pair in @(@($bs, $as, 'gone', $cb), @($as, $bs, 'added', $ca))) {
            foreach ($k in $pair[0]) {
                if ($pair[1].Contains($k)) { continue }
                if ($k.Contains($Control) -and $k.Contains($NCEpochDoc)) {
                    if ($t -eq 'documents' -and $pair[2] -eq 'added') { $epochNew = $true }
                    $script:controlChanged++; continue
                }
                if ($t -eq 'memory_units' -and $pair[3].Contains($k)) { $units[$pair[2]] += $k; continue }
                if ($t -eq 'memory_links' -and $k.EndsWith(",$Control)")) { $links[$pair[2]] += $k; continue }
                $out += "$($pair[2]): $t $k"
            }
        }
    }
    # The epoch's rewrite replaces its one unit: one gone and one added, or
    # one added and none gone when the document is new. Else every changed
    # control unit and link is a difference.
    $g = $units['gone'].Count; $n = $units['added'].Count
    $epochOk = $n -le 1 -and ($g -eq $n -or ($epochNew -and $g -eq 0))
    foreach ($dir in 'gone', 'added') {
        foreach ($k in $units[$dir]) {
            if ($epochOk) { $script:controlChanged++; continue }
            $out += "${dir}: memory_units $k (control units gone $($units['gone'].Count), added $($units['added'].Count): the epoch document's rewrite replaces one)"
        }
        foreach ($k in $links[$dir]) {
            $f = $k.TrimStart('(').Split(',')
            if ($epochOk -and ($units[$dir] -contains "($($f[0]))" -or $units[$dir] -contains "($($f[1]))")) { $script:controlChanged++; continue }
            $out += "${dir}: memory_links $k (a control link joining no unit of the epoch document)"
        }
    }
    return $out
}

# Compare-NcControlDocs: the control documents but the epoch's whose
# metadata differs or that are gone or new between two fingerprints.
function Compare-NcControlDocs($a, $b) {
    $out = @()
    $docs = @(@($a.control_documents.PSObject.Properties.Name) + @($b.control_documents.PSObject.Properties.Name) | Sort-Object -Unique)
    foreach ($d in $docs) {
        if ($d -eq $NCEpochDoc) { continue }
        if ($a.control_documents.$d -ne $b.control_documents.$d) { $out += "control document $d" }
    }
    return $out
}

# Test-NcSameKeys: no key of before gone, none added, but the epoch
# document's rewrite (Compare-NcKeys); every other control document's
# metadata as it was (the fingerprints fpBefore and fpAfter).
function Test-NcSameKeys([string]$Before, [string]$After, [string]$What, $FpBefore, $FpAfter) {
    $diff = @(Compare-NcKeys (Join-Path $script:work "$Before.keys") (Join-Path $script:work "$After.keys") "$($script:bank)-control")
    $diff += @(Compare-NcControlDocs $FpBefore $FpAfter)
    if ($diff.Count -gt 0) { Write-NcLog (Protect-NcText (($diff | Select-Object -First 40) -join "`n")); Fail "the data changed $What ($($diff.Count) key(s))" }
    return "$($script:controlChanged) key(s) of the control bank's epoch document rewritten"
}

function Get-NcSuperviseJson { return ([IO.File]::ReadAllText($script:superviseFile) | ConvertFrom-Json) }

# Get-NcPgPort: the database's port, from supervise.json's postgres argv.
function Get-NcPgPort {
    $a = @((Get-NcSuperviseJson).postgres)
    for ($i = 0; $i -lt $a.Count - 1; $i++) { if ($a[$i] -eq '-p') { return [int]$a[$i + 1] } }
    Fail "supervise.json's postgres has no -p"
}

function Get-NcPostmaster {
    $f = Join-Path $script:native 'db\postmaster.pid'
    if (-not (Test-Path -LiteralPath $f)) { return 0 }
    try { return [int]((Get-Content -LiteralPath $f -TotalCount 1).Trim()) } catch { return 0 }
}

function Test-NcPgReady {
    $code = Invoke-Nc -Name 'pg_isready' -Exe (Join-Path $script:pgBin 'pg_isready.exe') -Arguments @('-q', '-h', '127.0.0.1', '-p', [string](Get-NcPgPort)) -TimeoutSec 30
    return $code -eq 0
}

function Get-NcLogLength([string]$f) {
    if (Test-Path -LiteralPath $f) { return (Get-Item -LiteralPath $f).Length }
    return 0
}

# Read-NcLogSince: the log's text from offset on (a rotated log: all of it).
function Read-NcLogSince([string]$f, [long]$from) {
    if (-not (Test-Path -LiteralPath $f)) { return '' }
    # The supervisor holds its log open for writing: read it sharing that.
    $fs = New-Object IO.FileStream $f, ([IO.FileMode]::Open), ([IO.FileAccess]::Read), ([IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete)
    try {
        $ms = New-Object IO.MemoryStream
        $fs.CopyTo($ms)
        $raw = $ms.ToArray()
    } finally { $fs.Dispose() }
    if ($from -gt $raw.Length) { $from = 0 }
    return [Text.Encoding]::UTF8.GetString($raw, [int]$from, $raw.Length - [int]$from)
}

function Test-NcTask {
    return (Invoke-Nc -Name 'schtasks-query' -Exe 'schtasks.exe' -Arguments @('/Query', '/TN', $script:taskName)) -eq 0
}

# Get-NcStartPath: which start path the last start took: the detached
# supervisor says so on stderr (server.NoLogonNote), the task's runs in
# the console session.
function Get-NcStartPath([string]$Name) {
    $err = Read-NcText $Name 'err'
    $sup = @(Get-NcSupervisors)
    $where = ''
    if ($sup.Count -gt 0) { $where = " (supervisor pid $($sup[0].Id), session $($sup[0].SessionId); this shell's session $((Get-Process -Id $PID).SessionId))" }
    if ($err -match 'the server runs until you log off') { return "detached$where" }
    return "task$where"
}

function Stop-NcServer([string]$Name) {
    if ((Bs $Name @('server', 'stop', '--json')) -ne 0) { Show-Nc $Name; Fail 'server stop failed' }
    $left = @(Wait-NcGone @(Get-NcSupervisors | ForEach-Object { $_.Id }) 30)
    if ($left.Count -gt 0) { Fail "the supervisor (pid $($left -join ', ')) still runs after server stop" }
    $pg = @(Get-NcProcs $script:native 'postgres')
    if ($pg.Count -gt 0) { Fail "postgres.exe (pid $(@($pg | ForEach-Object { $_.Id }) -join ', ')) still runs after server stop" }
}

function Start-NcServer([string]$Name, [string]$What) {
    if ((Bs $Name @('server', 'start', '--json')) -ne 0) { Show-Nc $Name; Show-NcFile $script:supLog; Fail "server start $What failed" }
    [void](Wait-NcHealthy 120 "after server start $What")
}

# nhf: native's models from and to BACKSIGHT_NATIVE_HF (CI's cache). A
# failure is a warning: the setup downloads them.
$script:nhfRead = $false
function Copy-NcModelsIn {
    $src = $env:BACKSIGHT_NATIVE_HF
    if (-not $src) { return }
    if (-not (Test-Path -LiteralPath (Join-Path $src '.backsight-models'))) { Write-NcLog "native hf cache: $src holds no models yet, setup downloads them"; return }
    $script:nhfRead = $true
    try {
        New-Item -ItemType Directory -Force -Path (Join-Path $script:native 'hf') | Out-Null
        Copy-Item -Recurse -Force -Path (Join-Path $src '*') -Destination (Join-Path $script:native 'hf')
        Copy-Item -Force -LiteralPath (Join-Path $src '.backsight-models') -Destination (Join-Path $script:native 'hf')
        Write-NcLog "native hf cache: $($script:native)\hf seeded from $src"
    } catch {
        Write-NcLog "::warning::native hf cache: could not seed from ${src}: $($_.Exception.Message); setup downloads the models"
        Remove-Item -Recurse -Force -LiteralPath (Join-Path $script:native 'hf') -ErrorAction SilentlyContinue
    }
}
function Copy-NcModelsOut {
    $dst = $env:BACKSIGHT_NATIVE_HF
    $hf = Join-Path $script:native 'hf'
    if (-not $dst -or $script:nhfRead -or -not (Test-Path -LiteralPath (Join-Path $hf '.backsight-models'))) { return }
    try {
        New-Item -ItemType Directory -Force -Path $dst | Out-Null
        Copy-Item -Recurse -Force -Path (Join-Path $hf '*') -Destination $dst
        Copy-Item -Force -LiteralPath (Join-Path $hf '.backsight-models') -Destination $dst
        Write-NcLog "native hf cache: $hf saved to $dst"
    } catch { Write-NcLog "::warning::native hf cache: could not save $hf to ${dst}: $($_.Exception.Message)" }
}

function Get-NcUserPath([string]$Key) {
    $k = Get-Item -LiteralPath "HKCU:\$Key" -ErrorAction SilentlyContinue
    if ($null -eq $k) { return '' }
    $v = $k.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
    if ($null -eq $v) { return '' }
    return [string]$v
}

# ---- the steps ---------------------------------------------------------

function Step-build {
    $b = Join-Path $script:work 'build'
    New-Item -ItemType Directory -Force -Path $b | Out-Null
    $env:CGO_ENABLED = '0'
    $tr = Join-Path $b 'testrelease.exe'
    if ((Invoke-Nc -Name 'go-testrelease' -Exe 'go' -Arguments @('build', '-C', $script:repo, '-o', $tr, './scripts/testrelease')) -ne 0) { Show-Nc 'go-testrelease'; Fail 'building scripts/testrelease failed' }
    $key = Join-Path $script:work 'test.key'
    if ((Invoke-Nc -Name 'keygen' -Exe $tr -Arguments @('keygen', $key)) -ne 0) { Show-Nc 'keygen'; Fail 'testrelease keygen failed' }
    $script:pubkey = (Read-NcText 'keygen').Trim()
    if ($script:pubkey -notmatch '^RW[A-Za-z0-9+/=]{54}$') { Fail "testrelease keygen printed no public key line" }
    $ld = "-s -w -X $NCPkg/internal/buildinfo.Version=$NCVersion -X $NCPkg/internal/signing.LinkedTestKey=$($script:pubkey)"
    if ((Invoke-Nc -Name 'go-build' -Exe 'go' -Arguments @('build', '-C', $script:repo, '-trimpath', '-tags', 'testkey,testhooks', '-ldflags', $ld, '-o', $script:build, './cmd/backsight')) -ne 0) {
        Show-Nc 'go-build'; Fail 'go build of the testkey binary failed'
    }
    # The second build loads a copy of the Windows lock with its first line
    # changed: another lock hash, so another venv, of the same Hindsight.
    $lock = Join-Path $script:repo 'internal\server\nativelock\requirements-windows-amd64.txt'
    $lock2 = Join-Path $script:work 'lock2.txt'
    $lines = [IO.File]::ReadAllLines($lock)
    $lines[0] = '# Generated by scripts/native-lock.sh --platform windows: do not edit (native-chain-windows second lock).'
    [IO.File]::WriteAllText($lock2, (($lines -join "`n") + "`n"))
    if ([IO.File]::ReadAllText($lock2) -eq [IO.File]::ReadAllText($lock)) { Fail 'the second lock equals the first' }
    $ld2 = "$ld -X $NCPkg/internal/server/nativelock.TestLock=$lock2"
    if ((Invoke-Nc -Name 'go-build2' -Exe 'go' -Arguments @('build', '-C', $script:repo, '-trimpath', '-tags', 'testkey,testhooks', '-ldflags', $ld2, '-o', $script:bin2, './cmd/backsight')) -ne 0) {
        Show-Nc 'go-build2'; Fail 'go build of the second-lock binary failed'
    }
    if ((Invoke-Nc -Name 'release' -Exe $tr -Arguments @('release', $key, $script:build, $NCVersion, $script:release)) -ne 0) { Show-Nc 'release'; Fail 'testrelease release failed' }
    foreach ($f in 'checksums.txt', 'checksums.txt.minisig', "backsight_${NCVersion}_windows_amd64.zip") {
        if (-not (Test-Path -LiteralPath (Join-Path $script:release $f))) { Fail "the release has no $f" }
    }
    return "backsight $NCVersion (testkey, testhooks) and its second-lock twin built; release signed by a throwaway key in $($script:release)"
}

function Step-install {
    $ps = (Get-Process -Id $PID).Path
    $code = Invoke-Nc -Name 'install' -Exe $ps -Arguments @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $script:repo 'scripts\install.ps1')) `
        -Env @{ BACKSIGHT_RELEASE_DIR = $script:release; BACKSIGHT_INSTALL_PUBKEY = $script:pubkey }
    $out = (Read-NcText 'install') + (Read-NcText 'install' 'err')
    if ($code -ne 0) { Show-Nc 'install'; Fail "install.ps1 failed (exit $code)" }
    $want = "backsight $NCVersion installed: $($script:bin)"
    if (-not $out.Contains($want)) { Show-Nc 'install'; Fail "install.ps1 did not say '$want'" }
    if (-not $out.Contains("BACKSIGHT_INSTALL_PUBKEY is set: the release is checked against $($script:pubkey)")) { Show-Nc 'install'; Fail 'install.ps1 did not warn that it trusts the test key' }
    if ((Bs 'version' @('version')) -ne 0) { Show-Nc 'version'; Fail 'the installed backsight does not run' }
    $v = Read-NcText 'version'
    if (-not $v.Contains("backsight $NCVersion ") -or -not $v.Contains('test build: trusts the test signing key')) { Show-Nc 'version'; Fail "the installed backsight is not $NCVersion, the test build" }
    $up = Get-NcUserPath $script:userPathKey
    if (-not ";$up;".Contains(";$($script:bh)\bin;")) { Fail "the user PATH (throwaway key HKCU:\$($script:userPathKey)) lacks $($script:bh)\bin: '$up'" }
    if ((Get-NcUserPath 'Environment') -ne $script:realPath) { Fail 'install.ps1 changed the real user PATH: the throwaway key was not used' }
    return "install.ps1 installed $($script:bin) (backsight $NCVersion, test build) and put $($script:bh)\bin on the user PATH"
}

function Step-setup {
    if ((Bs 'secret' @('secret', 'set', 'llm_api_key') -Stdin "mock-not-a-key`n") -ne 0) { Show-Nc 'secret'; Fail 'secret set llm_api_key failed' }
    Copy-NcModelsIn
    $code = Bs 'setup' @('setup', '--runtime', 'native', '--non-interactive', '--json', '--answer', 'provider=mock', '--answer', 'model=mock-model', '--answer', 'consent=yes',
        '--answer', 'retention=3650', '--answer', "project=$($script:project)", '--answer', "api_port=$($script:port)", '--answer', "ui_port=$($script:port + 1111)", '--answer', 'scheduler=off')
    if ($code -ne 0) { Show-Nc 'setup'; Show-NcFile $script:supLog; Show-NcFile $script:dbLog; Show-NcFile $script:apiLog; Fail "setup --runtime native failed (exit $code)" }
    Copy-NcModelsOut
    $cfg = [IO.File]::ReadAllText((Join-Path $script:bh 'config\config.toml'))
    if ($cfg -notmatch '(?m)^runtime *= *"native"') { Fail 'config.toml does not say runtime native after setup --runtime native' }
    $script:bank = 'backsight'
    if ($cfg -match '(?m)^bank *= *"([^"]+)"') { $script:bank = $Matches[1] }
    if (-not (Test-Path -LiteralPath $script:superviseFile)) { Fail "no $($script:superviseFile) after setup" }
    if (-not (Test-NcTask)) { Fail "the task $($script:taskName) is not registered after setup" }
    if (-not (Test-NcHealthy)) { Show-Nc 'status'; Fail 'the server is not healthy after setup' }
    $script:startPath = Get-NcStartPath 'setup'
    Write-NcLog "native-chain-windows: start path after setup: $($script:startPath)"
    [void](Bs 'doctor' @('doctor', '--json'))
    $d = Read-NcJson 'doctor'
    if ($null -eq $d -or @($d.data.checks).Count -eq 0) { Show-Nc 'doctor'; Fail 'doctor listed no check' }
    $errs = @($d.data.checks | Where-Object { $_.status -eq 'error' })
    if ($errs.Count -gt 0) {
        Write-NcLog (Protect-NcText (($errs | ForEach-Object { $_ | ConvertTo-Json -Compress -Depth 5 }) -join "`n"))
        Fail 'doctor reports errors on a fresh native install'
    }
    $counts = (@($d.data.checks | Group-Object status | ForEach-Object { "$($_.Name) $($_.Count)" }) -join ', ')
    return "healthy, task $($script:taskName) registered, start path: $($script:startPath); doctor $counts"
}

# The VC++ runtime's DLLs (msvc-runtime's nine and Python's own).
$NCRuntimeDll = '^(msvcp140(_\d+|_atomic_wait|_codecvt_ids)?|vcruntime140(_1|_threads)?|concrt140|vcomp140|vccorlib140|vcamp140)\.dll$'

function Step-dlls {
    $sys = Join-Path $env:SystemRoot 'System32'
    $inSys = @(Get-ChildItem -LiteralPath $sys -Filter '*140*.dll' -ErrorAction SilentlyContinue | Where-Object { $_.Name -match $NCRuntimeDll } | ForEach-Object { $_.Name })
    $procs = @(Get-NcProcs $script:native)
    if ($procs.Count -eq 0) { Fail "no process runs from $($script:native)" }
    $local = @(); $foreign = @()
    $pgLoads = $false
    foreach ($p in $procs) {
        try { $mods = @($p.Modules) } catch { Fail "the modules of $($p.Name) (pid $($p.Id)) cannot be read: $($_.Exception.Message)" }
        foreach ($m in $mods) {
            if ($m.ModuleName -notmatch $NCRuntimeDll) { continue }
            $line = "$($p.Name)/$($p.Id): $($m.FileName)"
            if (Test-NcUnder $m.FileName $script:native) {
                $local += $line
                if ($p.Name -eq 'postgres' -and (Test-NcUnder $m.FileName $script:pgBin)) { $pgLoads = $true }
            } else { $foreign += $line }
        }
    }
    Write-NcLog ("native-chain-windows: VC++ runtime DLLs in System32: " + $(if ($inSys.Count) { $inSys -join ', ' } else { 'none' }))
    # One line per program and DLL: postgres runs a process per backend.
    $seen = @{}
    foreach ($l in $local) {
        $k = ($l -replace '/\d+:', ':')
        if (-not $seen.ContainsKey($k)) { $seen[$k] = 0 }
        $seen[$k]++
    }
    foreach ($k in ($seen.Keys | Sort-Object)) { Write-NcLog "  app-local $k ($($seen[$k]) process(es))" }
    if ($foreign.Count -gt 0) { Write-NcLog ($foreign -join "`n"); Fail 'a server process loaded the VC++ runtime from outside the native root' }
    if (-not $pgLoads) { Fail "no postgres.exe loaded the VC++ runtime from $($script:pgBin)" }
    $names = @($procs | ForEach-Object { $_.Name } | Sort-Object -Unique) -join ', '
    return "$($local.Count) runtime DLL loads, all app-local under the native root ($names); System32 holds $(if ($inSys.Count) { $inSys.Count } else { 'none' }) of them"
}

function Step-store {
    for ($i = 1; $i -le 3; $i++) {
        if ((Bs "store-$i" @('store', '--json', '--kind', 'decision', "Native chain note ${i}: the billing service keeps PostgreSQL 1$i with partial indexes.")) -ne 0) { Show-Nc "store-$i"; Fail "store $i failed" }
        $j = Read-NcJson "store-$i"
        if ($null -eq $j -or -not $j.ok -or $j.data.status -ne 'stored') { Show-Nc "store-$i"; Fail "note $i was not stored" }
    }
    for ($i = 1; $i -le 60; $i++) {
        [void](Bs 'search' @('search', '--all', '--min-score', '0', '--json', 'Which PostgreSQL version does billing keep?'))
        $j = Read-NcJson 'search'
        if ($null -ne $j -and $j.ok -and @($j.data.hits).Count -gt 0) { return "3 notes stored, the search found $(@($j.data.hits).Count) hit(s) after $i tries" }
        Start-Sleep -Seconds 3
    }
    Show-Nc 'search'
    Fail 'search never found the stored notes'
}

function Step-backup {
    # The data settled first (a stored note's extraction runs in the
    # background): the archive then holds exactly what restore must bring
    # back.
    $fpBackup = Get-NcFingerprint 'fp-backup'
    $script:fpBackup = $fpBackup
    Save-NcKeys 'keys-backup'
    if ((Bs 'backup' @('backup', 'create', '--json')) -ne 0) { Show-Nc 'backup'; Fail 'backup create failed' }
    $j = Read-NcJson 'backup'
    $script:archive = $j.data.path
    if (-not $j.ok -or $j.data.skipped -or -not $script:archive -or -not (Test-Path -LiteralPath $script:archive) -or (Get-Item -LiteralPath $script:archive).Length -eq 0) { Show-Nc 'backup'; Fail 'backup create wrote no archive' }
    if ((Bs 'backup-verify' @('backup', 'verify', '--json', $script:archive)) -ne 0) { Show-Nc 'backup-verify'; Fail 'backup verify failed' }
    $v = Read-NcJson 'backup-verify'
    $rows = 0
    foreach ($p in $v.data.restored.PSObject.Properties) { $rows += [int]$p.Value }
    if (-not $v.ok -or -not $v.data.ok -or $rows -le 0) { Show-Nc 'backup-verify'; Fail 'the backup restored no rows' }
    $after = Get-NcFingerprint 'fp-backup-after'
    $moved = @(Compare-NcFingerprint $fpBackup $after)
    if ($moved.Count -gt 0) { Write-NcLog ($moved -join "`n"); Fail 'the data changed while it was backed up: the restore below would prove nothing' }
    return "$(Split-Path -Leaf $script:archive) created and verified ($rows rows restored in a throwaway instance)"
}

# Test-NcDocumentKey: a documents key of the --keys file name names doc.
function Test-NcDocumentKey([string]$Name, [string]$Doc) {
    $k = Read-NcKeys (Join-Path $script:work "$Name.keys")
    if (-not $k.ContainsKey('documents')) { return $false }
    foreach ($x in $k['documents']) { if ($x.Contains($Doc)) { return $true } }
    return $false
}

function Step-restore {
    # A note the archive does not hold: the restore must remove it.
    if ((Bs 'store-after-backup' @('store', '--json', '--kind', 'decision', 'Native chain note 4: stored after the backup, gone after the restore.')) -ne 0) { Show-Nc 'store-after-backup'; Fail 'storing a note after the backup failed' }
    $doc = (Read-NcJson 'store-after-backup').data.document
    if (-not $doc) { Show-Nc 'store-after-backup'; Fail 'the note stored after the backup has no document id' }
    $seen = $false
    for ($i = 1; $i -le 30 -and -not $seen; $i++) {
        Save-NcKeys 'keys-after-backup'
        $seen = Test-NcDocumentKey 'keys-after-backup' $doc
        if (-not $seen) { Start-Sleep -Seconds 2 }
    }
    if (-not $seen) { Fail "the note stored after the backup ($doc) never reached the documents table" }
    $code = Bs 'restore' @('restore', '--json', '--non-interactive', $script:archive)
    $j = Read-NcJson 'restore'
    if ($code -ne 0 -or $null -eq $j -or -not $j.ok) { Show-Nc 'restore'; Fail "restore failed (exit $code)" }
    $done = @($j.data.steps | Where-Object { $_.status -eq 'done' } | ForEach-Object { $_.name })
    if ($j.data.rolled_back -or [int]$j.data.epoch.epoch -le 0 -or $done -notcontains 'counts' -or $done -notcontains 'recall' -or $done -notcontains 'epoch') {
        Show-Nc 'restore'; Fail 'the restore did not pass its checks (counts, recall, epoch)'
    }
    [void](Wait-NcHealthy 120 'after the restore')
    $fpRestore = Get-NcFingerprint 'fp-restore'
    Save-NcKeys 'keys-restore'
    if (Test-NcDocumentKey 'keys-restore' $doc) { Fail "the note stored after the backup ($doc) is still there after the restore: the archive was not restored" }
    $same = Test-NcSameKeys 'keys-backup' 'keys-restore' 'across the restore' $script:fpBackup $fpRestore
    return "restored $(Split-Path -Leaf $script:archive): epoch $($j.data.epoch.epoch), healthy, the backup's data, the note stored after it gone ($same)"
}

function Step-upgrade-rollback {
    $venv = (Get-NcSuperviseJson).venv
    if (-not $venv) { Fail 'supervise.json names no venv' }
    $fpBefore = Get-NcFingerprint 'fp-before-upgrade'
    Save-NcKeys 'keys-before-upgrade'
    $code = Invoke-Nc -Name 'upgrade' -Exe $script:bin2 -Arguments @('server', 'upgrade', '--allow-unordered', '--json') -Env @{ BACKSIGHT_TEST_FAIL_SMOKE = '1' }
    $j = Read-NcJson 'upgrade'
    # A rolled-back upgrade exits 30, RolledBack (ruling R5).
    if ($code -ne 30) { Show-Nc 'upgrade'; Show-NcFile $script:supLog; Fail "a failing smoke: exit $code, want 30" }
    if ($null -eq $j -or -not $j.data.rolled_back -or $j.error.message -notmatch 'BACKSIGHT_TEST_FAIL_SMOKE') { Show-Nc 'upgrade'; Fail 'the failed upgrade does not say it rolled back' }
    if ($j.data.current -ne $venv -or $j.data.target -eq $venv -or -not $j.data.target) { Show-Nc 'upgrade'; Fail "the upgrade did not go from $venv to the second lock's venv" }
    if (-not (Test-Path -LiteralPath (Join-Path $script:native "venv\$($j.data.target)\Scripts\hindsight-api.exe"))) { Fail 'the second venv was never installed' }
    $now = (Get-NcSuperviseJson).venv
    if ($now -ne $venv) { Fail "supervise.json runs $now after the rollback, want $venv" }
    [void](Wait-NcHealthy 120 'after the rollback')
    $venvDir = Join-Path $script:native "venv\$venv"
    $api = @(Get-NcProcs $script:native 'hindsight-api')
    if ($api.Count -eq 0) { Fail 'no hindsight-api.exe runs after the rollback' }
    $off = @($api | Where-Object { -not (Test-NcUnder $_.Path $venvDir) })
    if ($off.Count -gt 0) { Fail "hindsight-api.exe runs from $(@($off | ForEach-Object { $_.Path }) -join ', ') after the rollback, not from $venvDir" }
    $fpAfter = Get-NcFingerprint 'fp-after-upgrade'
    Save-NcKeys 'keys-after-upgrade'
    $same = Test-NcSameKeys 'keys-before-upgrade' 'keys-after-upgrade' 'across the rolled-back upgrade' $fpBefore $fpAfter
    return "exit 30, rolled back to $venv from $($j.data.target), hindsight-api.exe runs from it, healthy, the same data ($same)"
}

function Step-kill-postgres {
    $pm = Get-NcPostmaster
    if ($pm -le 0 -or -not (Test-NcAlive $pm)) { Fail "postmaster.pid names no running postgres ($pm)" }
    $t = Get-Date
    Stop-Process -Id $pm -Force
    $back = 0
    while (((Get-Date) - $t).TotalSeconds -lt 30) {
        $now = Get-NcPostmaster
        if ($now -gt 0 -and $now -ne $pm -and (Test-NcAlive $now) -and (Test-NcPgReady)) { $back = $now; break }
        Start-Sleep -Milliseconds 500
    }
    $secs = [int]((Get-Date) - $t).TotalSeconds
    if ($back -eq 0) { Show-NcFile $script:supLog; Show-NcFile $script:dbLog; Fail "no postgres answers within 30 s of killing the postmaster $pm" }
    $h = Wait-NcHealthy 120 'after the postmaster was killed'
    return "postmaster $pm killed; postmaster $back answers after $secs s, healthy $h s later"
}

function Step-kill-supervisor {
    $sup = @(Get-NcSupervisors)
    if ($sup.Count -ne 1) { Fail "$($sup.Count) supervisors run, want 1" }
    $kids = @(Get-NcProcs $script:native | Where-Object { @('postgres', 'hindsight-api', 'python') -contains $_.Name })
    $pg = @($kids | Where-Object { $_.Name -eq 'postgres' })
    $api = @($kids | Where-Object { $_.Name -eq 'hindsight-api' })
    if ($pg.Count -eq 0 -or $api.Count -eq 0) { Fail "the supervisor runs $($pg.Count) postgres.exe and $($api.Count) hindsight-api.exe, want both" }
    $ids = @($kids | ForEach-Object { $_.Id })
    Stop-Process -Id $sup[0].Id -Force
    $left = @(Wait-NcGone $ids 10)
    if ($left.Count -gt 0) { Fail "killing the supervisor $($sup[0].Id) left pid $($left -join ', ') running" }
    $others = @(Get-NcProcs $script:native | Where-Object { @('postgres', 'hindsight-api') -contains $_.Name })
    if ($others.Count -gt 0 -and @(Get-NcSupervisors).Count -eq 0) { Fail "postgres.exe or hindsight-api.exe (pid $(@($others | ForEach-Object { $_.Id }) -join ', ')) runs without a supervisor" }
    $from = Get-NcLogLength $script:supLog
    Start-NcServer 'start-after-kill' 'after the supervisor was killed'
    $log = Read-NcLogSince $script:supLog $from
    $said = (Read-NcText 'start-after-kill') + (Read-NcText 'start-after-kill' 'err') + $log
    if ($said -match 'a postgres \(pid \d+\) runs on') { Write-NcLog (Protect-NcText $said); Fail 'server start after the killed supervisor reported a stray' }
    return "supervisor $($sup[0].Id) killed: its $($ids.Count) children (postgres, hindsight-api, python) gone; server start healthy (start path: $(Get-NcStartPath 'start-after-kill')), no stray reported"
}

function Step-refuse-identity {
    $script:fpRefusals = Get-NcFingerprint 'fp-before-refusals'
    Stop-NcServer 'stop-identity'
    $idf = Join-Path $script:native 'identity'
    $kept = [IO.File]::ReadAllText($idf)
    $other = '7000000000000000001'
    if ($kept.Trim() -eq $other) { $other = '7000000000000000002' }
    [IO.File]::WriteAllText($idf, "$other`n")
    try {
        $code = Bs 'start-identity' @('server', 'start', '--json')
        $said = (Read-NcText 'start-identity') + (Read-NcText 'start-identity' 'err')
        if ($code -eq 0) { Fail 'server start accepted a database whose identity is not the recorded one' }
        if ($said -notmatch 'is not the one backsight recorded') { Show-Nc 'start-identity'; Fail 'the refusal does not say the identity is not the recorded one' }
        Start-Sleep -Seconds $NCSettle # a start the task runs after the refusal would show by now
        $sup = @(Get-NcSupervisors); $pg = @(Get-NcProcs $script:native 'postgres')
        if ($sup.Count -gt 0 -or $pg.Count -gt 0) { Fail "the refused start started something: $($sup.Count) supervisor(s), $($pg.Count) postgres.exe" }
        [void](Bs 'status-identity' @('server', 'status', '--json'))
        $st = (Read-NcText 'status-identity') + (Read-NcText 'status-identity' 'err')
        if ($st -notmatch 'is not the one backsight recorded') { Show-Nc 'status-identity'; Fail 'server status does not give the identity refusal' }
    } finally {
        [IO.File]::WriteAllText($idf, $kept)
    }
    Start-NcServer 'start-identity-back' 'with the identity put back'
    return "server start refused exit $code, nothing started, server status gives the reason; identity put back, healthy"
}

# Move-NcDir renames a directory, retried a few seconds while denied (an
# indexer or an antivirus holding a file a moment).
function Move-NcDir([string]$From, [string]$To) {
    for ($i = 0; ; $i++) {
        try { [IO.Directory]::Move($From, $To); return } catch { if ($i -ge 20) { throw } }
        Start-Sleep -Milliseconds 500
    }
}

function Step-refuse-missing {
    Stop-NcServer 'stop-missing'
    $db = Join-Path $script:native 'db'
    $aside = Join-Path $script:work 'db-aside'
    Move-NcDir $db $aside
    try {
        $code = Bs 'start-missing' @('server', 'start', '--json')
        $said = (Read-NcText 'start-missing') + (Read-NcText 'start-missing' 'err')
        if ($code -eq 0) { Fail 'server start accepted a missing database' }
        if ($said -notmatch 'is missing: refusing to start') { Show-Nc 'start-missing'; Fail 'the refusal does not say the database is missing' }
        Start-Sleep -Seconds $NCSettle
        $sup = @(Get-NcSupervisors); $pg = @(Get-NcProcs $script:native 'postgres')
        if ($sup.Count -gt 0 -or $pg.Count -gt 0) { Fail "the refused start started something: $($sup.Count) supervisor(s), $($pg.Count) postgres.exe" }
        if (Test-Path -LiteralPath $db) { Fail "the refused start made a database at $db in place of the missing one" }
        [void](Bs 'status-missing' @('server', 'status', '--json'))
        $st = (Read-NcText 'status-missing') + (Read-NcText 'status-missing' 'err')
        if ($st -notmatch 'is missing: refusing to start') { Show-Nc 'status-missing'; Fail 'server status does not give the missing-database refusal' }
    } finally {
        if (-not (Test-Path -LiteralPath $db)) { Move-NcDir $aside $db }
    }
    Start-NcServer 'start-missing-back' 'with the database put back'
    return "server start refused exit $code (the database is missing), nothing started, server status gives the reason; put back, healthy"
}

function Step-refuse-stray {
    Stop-NcServer 'stop-stray'
    $port = Get-NcPgPort
    $log = Join-Path $script:work 'stray.log'
    $code = Invoke-Nc -Name 'stray' -Exe (Join-Path $script:pgBin 'pg_ctl.exe') -Arguments @('start', '-D', (Join-Path $script:native 'db'), '-l', $log, '-w', '-t', '60',
        '-o', "-p $port -c listen_addresses=127.0.0.1") -TimeoutSec 120 -NoCapture
    if ($code -ne 0) { Show-NcFile $log; Fail "pg_ctl start of a stray postgres failed (exit $code)" }
    $stray = Get-NcPostmaster
    if ($stray -le 0 -or (Get-Process -Id $stray -ErrorAction SilentlyContinue).Name -ne 'postgres') { Fail "postmaster.pid does not name the stray postgres ($stray)" }
    if (-not (Test-NcPgReady)) { Show-NcFile $log; Fail 'the stray postgres never answered' }
    $from = Get-NcLogLength $script:supLog
    $started = $false
    try {
        Start-NcServer 'start-stray' 'beside a stray postgres'
        $started = $true
    } finally {
        # Killed here only when server start failed: else the product
        # must have stopped it, checked below before any kill.
        if (-not $started -and (Test-NcAlive $stray)) { Stop-Process -Id $stray -Force -ErrorAction SilentlyContinue }
    }
    $left = @(Wait-NcGone @($stray) 5)
    if ($left.Count -gt 0) {
        Stop-Process -Id $stray -Force -ErrorAction SilentlyContinue
        Fail "the stray postgres $stray still runs after server start: server start did not stop it"
    }
    $said = (Read-NcText 'start-stray') + (Read-NcText 'start-stray' 'err') + (Read-NcLogSince $script:supLog $from)
    if ($said -notmatch "a postgres \(pid $stray\) runs on .*stopping it") { Write-NcLog (Protect-NcText $said); Fail "the stray $stray was not reported (server start's output, the supervisor's log)" }
    $pm = Get-NcPostmaster
    $sup = @(Get-NcSupervisors)
    if ($pm -eq $stray -or $sup.Count -ne 1) { Fail "the postmaster ($pm) is not a new one under one supervisor ($($sup.Count))" }
    return "stray postgres $stray (pg_ctl, outside the job) reported and stopped; postmaster $pm under supervisor $($sup[0].Id), healthy"
}

function Step-refuse-fsync {
    Stop-NcServer 'stop-fsync'
    $kept = [IO.File]::ReadAllText($script:superviseFile)
    if (-not $kept.Contains('"fsync=on"')) { Fail 'supervise.json has no fsync=on to change' }
    [IO.File]::WriteAllText($script:superviseFile, $kept.Replace('"fsync=on"', '"fsync=off"'), (New-Object Text.UTF8Encoding $false))
    $how = ''
    try {
        # The supervisor alone, as the task starts it: server start would
        # render supervise.json again.
        [void](Invoke-Nc -Name 'task-enable' -Exe 'schtasks.exe' -Arguments @('/Change', '/TN', $script:taskName, '/ENABLE'))
        [void](Invoke-Nc -Name 'task-run' -Exe 'schtasks.exe' -Arguments @('/Run', '/TN', $script:taskName))
        $t = Get-Date
        while (@(Get-NcSupervisors).Count -eq 0 -and ((Get-Date) - $t).TotalSeconds -lt 15) { Start-Sleep -Milliseconds 300 }
        if (@(Get-NcSupervisors).Count -gt 0) { $how = 'the task' } else {
            # No console logon: the task's command, detached by hand.
            $psi = New-Object Diagnostics.ProcessStartInfo
            $psi.FileName = Join-Path $script:bh 'data\bin\current.exe'
            $psi.Arguments = Join-NcArgs @('server', 'supervise', '--backsight-home', $script:bh)
            $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true
            [void][Diagnostics.Process]::Start($psi)
            $how = 'the shim, started by hand (the task did not run)'
        }
        $refusal = ''
        $t = Get-Date
        while (((Get-Date) - $t).TotalSeconds -lt 30) {
            [void](Bs 'status-fsync' @('server', 'status', '--json'))
            $st = (Read-NcText 'status-fsync') + (Read-NcText 'status-fsync' 'err')
            if ($st -match 'not fsync=on') { $refusal = $st; break }
            Start-Sleep -Seconds 1
        }
        if (-not $refusal) { Show-Nc 'status-fsync'; Show-NcFile $script:supLog; Fail 'server status never gave the fsync refusal' }
        if (-not $refusal.Contains('supervise.json')) { Show-Nc 'status-fsync'; Fail 'the fsync refusal does not name supervise.json' }
        Start-Sleep -Seconds $NCSettle
        $pg = @(Get-NcProcs $script:native 'postgres')
        if ($pg.Count -gt 0) { Fail "postgres.exe (pid $(@($pg | ForEach-Object { $_.Id }) -join ', ')) runs with fsync=off in supervise.json" }
        Stop-NcServer 'stop-fsync-refused'
    } finally {
        [IO.File]::WriteAllText($script:superviseFile, $kept, (New-Object Text.UTF8Encoding $false))
    }
    Start-NcServer 'start-fsync-back' 'after the fsync refusal'
    $fp = Get-NcFingerprint 'fp-after-refusals'
    $diff = @(Compare-NcFingerprint $script:fpRefusals $fp)
    if ($diff.Count -gt 0) { Write-NcLog ($diff -join "`n"); Fail 'the data changed across the refusals' }
    return "supervisor started by $how refused fsync=off (server status names supervise.json), no postgres.exe; healthy after, the data as before the refusals"
}

function Step-uninstall {
    $bank = $script:bank
    # The built binary, not the installed one: uninstall removes the
    # installed copies, which a running uninstall would hold open.
    $code = Invoke-Nc -Name 'uninstall' -Exe $script:build -Arguments @('uninstall', '--delete-data', '--non-interactive', '--json', '--answer', "confirm_bank=$bank",
        '--answer', 'identity_saved=yes', '--answer', 'drop_queued=yes', '--answer', 'pre_images_ack=yes')
    if ($code -ne 0) { Show-Nc 'uninstall'; Fail "uninstall --delete-data failed (exit $code)" }
    if (Test-NcTask) { Fail "schtasks /Query /TN $($script:taskName) still finds the task" }
    $procs = @(Get-NcProcs $script:bh)
    if ($procs.Count -gt 0) { Fail "processes still run from $($script:bh): $(@($procs | ForEach-Object { "$($_.Name)/$($_.Id)" }) -join ', ')" }
    if (Test-Path -LiteralPath $script:native) { Fail "the native root $($script:native) is still there" }
    $up = Get-NcUserPath $script:userPathKey
    if (";$up;".Contains(";$($script:bh)\bin;")) { Fail "$($script:bh)\bin is still on the user PATH (throwaway key): '$up'" }
    if ((Get-NcUserPath 'Environment') -ne $script:realPath) { Fail 'the real user PATH changed' }
    if (-not $script:pg0Before -and (Test-Path -LiteralPath $script:pg0Home)) {
        $in = @(Get-ChildItem -LiteralPath $script:pg0Home -Recurse -Force -ErrorAction SilentlyContinue | Select-Object -First 30 | ForEach-Object { $_.FullName.Substring($script:pg0Home.Length + 1) + $(if ($_.PSIsContainer) { '\' } else { " ($($_.Length) bytes)" }) }) -join ', '
        Fail "$($script:pg0Home), which this run's install created, is still there, holding: $in"
    }
    # What uninstall keeps (docs/uninstall.md): config, secrets and state,
    # and the backups; nothing else, not an empty directory.
    $kept = @('config', 'state', 'data', 'data\backups')
    $left = @(Get-ChildItem -LiteralPath $script:bh -Recurse -Force -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName.Substring($script:bh.Length + 1) } |
        Where-Object { $kept -notcontains $_ -and $_ -notlike 'config\*' -and $_ -notlike 'state\*' -and $_ -notlike 'data\backups\*' })
    if ($left.Count -gt 0) { Fail "uninstall --delete-data left $($left.Count) path(s) besides config\, state\ and data\backups\: $(($left | Select-Object -First 30) -join ', ')" }
    $backups = @(Get-ChildItem -LiteralPath (Join-Path $script:bh 'data\backups') -File -Force -ErrorAction SilentlyContinue).Count
    return "uninstalled: no task, no process, no native root, off the user PATH; nothing left but config\, state\ and $backups backup file(s)"
}

# ---- the run -----------------------------------------------------------

# Remove-NcTasks deletes the project's tasks and their folder.
function Remove-NcTasks {
    foreach ($t in 'server', 'tick', 'maintain') {
        [void](Invoke-Nc -Name 'cleanup-task' -Exe 'schtasks.exe' -Arguments @('/Delete', '/F', '/TN', "\$($script:project)\$t"))
    }
    try {
        $s = New-Object -ComObject Schedule.Service
        $s.Connect()
        $s.GetFolder('\').DeleteFolder($script:project, 0)
    } catch { }
}

# Invoke-NcCleanup: nothing in it fails or changes the run's status.
function Invoke-NcCleanup([bool]$failed) {
    $ErrorActionPreference = 'Continue'
    if ($failed -and $script:bh) {
        foreach ($f in $script:supLog, $script:dbLog, $script:apiLog) { try { Show-NcFile $f 40 } catch { } }
    }
    if ($env:KEEP -eq '1') { Write-NcLog "KEEP=1: $($script:work) and the project's tasks are left"; return }
    try { if ($script:bh) { Get-NcProcs $script:bh | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue } } } catch { }
    try { if ($script:work) { Get-NcProcs $script:work | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue } } } catch { }
    try { Remove-NcTasks } catch { }
    try { if ($script:userPathKey) { Remove-Item -LiteralPath "HKCU:\$($script:userPathKey)" -Recurse -Force -ErrorAction SilentlyContinue } } catch { }
    try {
        if ($null -ne $script:realPath -and (Get-NcUserPath 'Environment') -ne $script:realPath) {
            Set-ItemProperty -LiteralPath 'HKCU:\Environment' -Name Path -Value $script:realPath -Type $script:realPathKind
            Write-NcLog 'the real user PATH had changed: put back'
        }
    } catch { }
    try { if ($script:pg0Home -and -not $script:pg0Before -and (Test-Path -LiteralPath $script:pg0Home)) { Remove-Item -Recurse -Force -LiteralPath $script:pg0Home } } catch { }
    try { if ($script:claudeHome -and -not $script:claudeBefore -and (Test-Path -LiteralPath $script:claudeHome)) { Remove-Item -Recurse -Force -LiteralPath $script:claudeHome } } catch { }
    if ($script:work -and (Test-Path -LiteralPath $script:work)) {
        Start-Sleep -Seconds 1
        # Windows PowerShell's Remove-Item -Recurse fails on deep trees (the
        # venvs): cmd's rd does not.
        for ($i = 0; $i -lt 10 -and (Test-Path -LiteralPath $script:work); $i++) {
            & cmd.exe /d /c rd /s /q "$($script:work)" 2>$null | Out-Null
            if (Test-Path -LiteralPath $script:work) { Start-Sleep -Seconds 2 }
        }
        if (Test-Path -LiteralPath $script:work) { Write-NcLog "::warning::$($script:work) could not be removed" }
    }
}

# Invoke-NcSteps runs the steps in order, each a named check: a failure
# names its step and skips the rest. True when every step passed.
function Invoke-NcSteps([string[]]$Steps) {
    $failed = ''
    $script:results = @()
    foreach ($s in $Steps) {
        if ($failed) { $script:results += , @($s, 'SKIP', '-', "after $failed failed"); continue }
        Write-NcLog "native-chain-windows: step $s"
        $t = Get-Date
        try {
            $out = @(& "Step-$s")
            $detail = if ($out.Count) { Protect-NcText ([string]$out[-1]) } else { '' }
            $script:results += , @($s, 'PASS', "$([int]((Get-Date) - $t).TotalSeconds)s", $detail)
        } catch {
            Write-NcLog (Protect-NcText "native-chain-windows: step $s failed: $($_.Exception.Message)")
            $script:results += , @($s, 'FAIL', "$([int]((Get-Date) - $t).TotalSeconds)s", 'see above')
            $failed = $s
        }
    }
    Write-NcLog "native-chain-windows results (project $($script:project)):"
    foreach ($r in $script:results) { Write-NcLog ('{0,-17} {1,-4} {2,6}  {3}' -f $r[0], $r[1], $r[2], $r[3]) }
    if ($failed) { Write-NcLog "native-chain-windows: step $failed failed"; return $false }
    return $true
}

function Invoke-NcMain([string[]]$argv) {
    $ErrorActionPreference = 'Stop'
    $script:project = $env:PROJECT
    if (-not $script:project) { $script:project = 'bs-native-chain' }
    try { Test-NcProject $script:project; $script:port = Get-NcSlotPort 28750 $(if ($argv.Count) { $argv[0] } else { '' }) }
    catch { Write-NcLog $_.Exception.Message; return 2 }
    if ($env:NC_DRY -eq '1') { $NCSteps | ForEach-Object { [Console]::Out.WriteLine($_) }; return 0 }
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT -or $env:PROCESSOR_ARCHITECTURE -ne 'AMD64') { Write-NcLog 'native-chain-windows: the native runtime runs on Windows x86_64 only here'; return 2 }
    $script:repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
    $parent = $env:WORK
    if (-not $parent) { $parent = $env:RUNNER_TEMP }
    if (-not $parent) { $parent = [IO.Path]::GetTempPath() }
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    $script:work = Join-Path (Resolve-Path $parent).Path ('nc-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
    New-Item -ItemType Directory -Path $script:work | Out-Null
    $script:bh = Join-Path $script:work 'bs'
    $script:bin = Join-Path $script:bh 'bin\backsight.exe'
    $script:build = Join-Path $script:work 'build\backsight.exe'
    $script:bin2 = Join-Path $script:work 'build\backsight-lock2.exe'
    $script:release = Join-Path $script:work 'release'
    $script:native = Join-Path $script:bh 'data\server\native'
    $script:pgBin = Join-Path $script:native 'pg\18.1.0\bin'
    $script:superviseFile = Join-Path $script:native 'supervise.json'
    $script:taskName = "\$($script:project)\server"
    $logs = Join-Path $script:bh 'state\logs'
    $script:supLog = Join-Path $logs "$($script:project)-supervisor.log"
    $script:dbLog = Join-Path $logs "$($script:project)-db.log"
    $script:apiLog = Join-Path $logs "$($script:project)-api.log"
    $script:userPathKey = "Software\backsight-native-chain-$($script:project)"
    $envKey = Get-Item -LiteralPath 'HKCU:\Environment'
    $script:realPath = Get-NcUserPath 'Environment'
    $script:realPathKind = 'String'
    if ($envKey.GetValueNames() -contains 'Path') { $script:realPathKind = $envKey.GetValueKind('Path') }
    $script:pg0Home = Join-Path $env:USERPROFILE '.pg0'
    $script:pg0Before = Test-Path -LiteralPath $script:pg0Home
    $script:claudeHome = Join-Path $env:USERPROFILE '.claude'
    $script:claudeBefore = Test-Path -LiteralPath $script:claudeHome
    # The run's environment: its own BACKSIGHT_HOME and Claude Code
    # profile, the throwaway user PATH key, the stand-in claude first on
    # PATH.
    $fake = Join-Path $script:work 'fakebin'
    New-Item -ItemType Directory -Force -Path $fake, (Join-Path $script:work 'claude') | Out-Null
    [IO.File]::WriteAllText((Join-Path $fake 'claude.cmd'), "@echo off`r`nif `"%1`"==`"--version`" (echo 2.1.400 (Claude Code)& exit /b 0)`r`nif `"%3`"==`"list`" (echo []& exit /b 0)`r`nif `"%2`"==`"list`" (echo []& exit /b 0)`r`nexit /b 0`r`n")
    $env:BACKSIGHT_HOME = $script:bh
    $env:CLAUDE_CONFIG_DIR = Join-Path $script:work 'claude'
    $env:BACKSIGHT_TEST_USERPATH_KEY = $script:userPathKey
    $env:DISABLE_AUTOUPDATER = '1'
    $env:NO_COLOR = '1'
    $env:PATH = "$fake;$($env:PATH)"
    Remove-Item Env:BACKSIGHT_STATE_DIR -ErrorAction SilentlyContinue
    Write-NcLog "native-chain-windows: project $($script:project), API port $($script:port), run directory $($script:work)"
    $ok = $false
    try {
        Remove-NcTasks # a killed run's, of this project only
        $ok = Invoke-NcSteps $NCSteps
    } finally {
        Invoke-NcCleanup (-not $ok)
    }
    if (-not $ok) { return 1 }
    Write-NcLog "native-chain-windows: ok ($($NCSteps -join ' ')) on project $($script:project)"
    return 0
}

if ($env:NC_LIB -ne '1') {
    $code = @(Invoke-NcMain $args)
    exit ([int]$code[-1])
}
