# backsight bootstrap for Windows (decision 0080), install.sh's twin:
# downloads the release for this machine, checks its minisign signature
# against the key below and its checksum, and has the verified binary
# install itself under <data>\bin\<version> with the signed checksums.txt
# (it re-verifies itself against it), point the shim <data>\bin\current.exe
# at it, copy the shim to the command on PATH, <bin>\backsight.exe, and add
# <bin> to the user PATH; then says what to run next. No admin rights.
# Windows PowerShell 5.1 or PowerShell 7, nothing else: neither has
# Ed25519 or BLAKE2b, so the signature check is written here, in plain
# PowerShell over System.Numerics.BigInteger.
#
#   powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/Barben360/backsight/main/scripts/install.ps1 | iex"
#
# BACKSIGHT_RELEASE_DIR  a local directory holding a release (checksums.txt,
#                        its .minisig and the archives): no network
# BACKSIGHT_RELEASE_URL  where the release files are, as
#                        https://github.com/Barben360/backsight/releases/download/<tag>
#                        (default: the newest release, pre-releases included,
#                        found through the GitHub API without a token)
# BACKSIGHT_HOME         as for backsight itself: one directory for everything
# BACKSIGHT_INSTALL_PUBKEY  tests only: another trusted key
# BACKSIGHT_INSTALL_GITHUB  tests only: one https host standing in for
#                        api.github.com and github.com
# BACKSIGHT_INSTALL_LIB  tests only: 1 defines the functions below and
#                        installs nothing (the tests dot-source the script)
#
# The test-only overrides stay in the published script so that its tests
# run the very script users get. They are read from the environment of
# whoever runs it, never from the network or a file, and whoever sets that
# environment already controls this PowerShell. Either trust override set,
# the script says so on stderr, naming the key or host, so nobody installs
# unknowingly from another source (security review L3).
#
# Where the binary goes is the binary's own business (__install-shim, the
# directories backsight itself resolves, BACKSIGHT_HOME included), and so
# is the swap of a shim a running session may hold: this script verifies
# and hands over, it re-implements neither.

# BLAKE2b-512 (RFC 7693), minisign's prehash. PowerShell turns an integer
# overflow into a double, so each 64-bit word is two 32-bit halves held in
# Int64s: v[2i] the low half, v[2i+1] the high one.
$script:B2M = [long]4294967295
$script:B2IV = [long[]]@('6a09e667f3bcc908', 'bb67ae8584caa73b', '3c6ef372fe94f82b', 'a54ff53a5f1d36f1',
    '510e527fade682d1', '9b05688c2b3e6c1f', '1f83d9abfb41bd6b', '5be0cd19137e2179' | ForEach-Object {
        [Convert]::ToInt64($_.Substring(8), 16); [Convert]::ToInt64($_.Substring(0, 8), 16) })
$script:B2Sigma = @(
    @(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
    @(14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3),
    @(11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4),
    @(7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8),
    @(9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13),
    @(2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9),
    @(12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11),
    @(13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10),
    @(6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5),
    @(10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0))
# The G function's (a, b, c, d) word quadruples, in round order.
$script:B2G = @(@(0, 4, 8, 12), @(1, 5, 9, 13), @(2, 6, 10, 14), @(3, 7, 11, 15),
    @(0, 5, 10, 15), @(1, 6, 11, 12), @(2, 7, 8, 13), @(3, 4, 9, 14))

# Compress one 128-byte block of $buf at $off into $h (16 halves).
function Invoke-Blake2bCompress([long[]]$h, [byte[]]$buf, [int]$off, [long]$count, [bool]$last) {
    $mask = $script:B2M
    $m = New-Object 'long[]' 32
    for ($i = 0; $i -lt 32; $i++) { $m[$i] = [long][BitConverter]::ToUInt32($buf, $off + 4 * $i) }
    $v = New-Object 'long[]' 32
    for ($i = 0; $i -lt 16; $i++) { $v[$i] = $h[$i]; $v[$i + 16] = $script:B2IV[$i] }
    # the byte counter, low 64 bits (a release file is far below 2^32)
    $v[24] = $v[24] -bxor ($count -band $mask)
    $v[25] = $v[25] -bxor (($count -shr 32) -band $mask)
    if ($last) { $v[28] = $v[28] -bxor $mask; $v[29] = $v[29] -bxor $mask }
    for ($r = 0; $r -lt 12; $r++) {
        $s = $script:B2Sigma[$r % 10]
        for ($g = 0; $g -lt 8; $g++) {
            $q = $script:B2G[$g]
            $a = 2 * $q[0]; $b = 2 * $q[1]; $c = 2 * $q[2]; $d = 2 * $q[3]
            $x = 2 * $s[2 * $g]; $y = 2 * $s[2 * $g + 1]
            # a = a + b + m[x]; d = (d ^ a) >>> 32
            $lo = $v[$a] + $v[$b] + $m[$x]
            $v[$a + 1] = ($v[$a + 1] + $v[$b + 1] + $m[$x + 1] + ($lo -shr 32)) -band $mask
            $v[$a] = $lo -band $mask
            $xl = $v[$d] -bxor $v[$a]; $xh = $v[$d + 1] -bxor $v[$a + 1]
            $v[$d] = $xh; $v[$d + 1] = $xl
            # c = c + d; b = (b ^ c) >>> 24
            $lo = $v[$c] + $v[$d]
            $v[$c + 1] = ($v[$c + 1] + $v[$d + 1] + ($lo -shr 32)) -band $mask
            $v[$c] = $lo -band $mask
            $xl = $v[$b] -bxor $v[$c]; $xh = $v[$b + 1] -bxor $v[$c + 1]
            $v[$b] = (($xl -shr 24) -bor ($xh -shl 8)) -band $mask
            $v[$b + 1] = (($xh -shr 24) -bor ($xl -shl 8)) -band $mask
            # a = a + b + m[y]; d = (d ^ a) >>> 16
            $lo = $v[$a] + $v[$b] + $m[$y]
            $v[$a + 1] = ($v[$a + 1] + $v[$b + 1] + $m[$y + 1] + ($lo -shr 32)) -band $mask
            $v[$a] = $lo -band $mask
            $xl = $v[$d] -bxor $v[$a]; $xh = $v[$d + 1] -bxor $v[$a + 1]
            $v[$d] = (($xl -shr 16) -bor ($xh -shl 16)) -band $mask
            $v[$d + 1] = (($xh -shr 16) -bor ($xl -shl 16)) -band $mask
            # c = c + d; b = (b ^ c) >>> 63
            $lo = $v[$c] + $v[$d]
            $v[$c + 1] = ($v[$c + 1] + $v[$d + 1] + ($lo -shr 32)) -band $mask
            $v[$c] = $lo -band $mask
            $xl = $v[$b] -bxor $v[$c]; $xh = $v[$b + 1] -bxor $v[$c + 1]
            $v[$b] = (($xl -shl 1) -bor ($xh -shr 31)) -band $mask
            $v[$b + 1] = (($xh -shl 1) -bor ($xl -shr 31)) -band $mask
        }
    }
    for ($i = 0; $i -lt 16; $i++) { $h[$i] = $h[$i] -bxor $v[$i] -bxor $v[$i + 16] }
}

function Get-Blake2b512([byte[]]$data) {
    if ($null -eq $data) { $data = [byte[]]@() }
    $h = [long[]]$script:B2IV.Clone()
    $h[0] = $h[0] -bxor 0x01010040 # parameter block: digest 64 bytes, no key, fanout and depth 1
    $n = $data.Length
    $full = 0
    if ($n -gt 0) { $full = [int][Math]::Floor(($n - 1) / 128) } # the last block, even when full, is compressed last
    for ($k = 0; $k -lt $full; $k++) { Invoke-Blake2bCompress $h $data (128 * $k) (128 * ($k + 1)) $false }
    $tail = New-Object byte[] 128
    [Array]::Copy($data, 128 * $full, $tail, 0, $n - 128 * $full)
    Invoke-Blake2bCompress $h $tail 0 $n $true
    $out = New-Object byte[] 64
    for ($i = 0; $i -lt 16; $i++) {
        [Array]::Copy([BitConverter]::GetBytes([uint32]$h[$i]), 0, $out, 4 * $i, 4)
    }
    return , $out
}

# Ed25519 verification (RFC 8032), as libsodium and Go check it:
# cofactorless, S below L, R compared as an encoding. Points are extended
# coordinates (X, Y, Z, T) of BigIntegers kept reduced in (-p, p).
try { Add-Type -AssemblyName System.Numerics } catch { }
$script:EdP = [System.Numerics.BigInteger]::Pow(2, 255) - 19
$script:EdL = [System.Numerics.BigInteger]::Pow(2, 252) + [System.Numerics.BigInteger]::Parse('27742317777372353535851937790883648493')
$script:EdD = [System.Numerics.BigInteger]::Remainder(-121665 * [System.Numerics.BigInteger]::ModPow(121666, $script:EdP - 2, $script:EdP), $script:EdP) + $script:EdP
$script:EdD2 = [System.Numerics.BigInteger]::Remainder(2 * $script:EdD, $script:EdP)
$script:EdI = [System.Numerics.BigInteger]::ModPow(2, [System.Numerics.BigInteger]::Divide($script:EdP - 1, 4), $script:EdP) # sqrt(-1)

function ConvertTo-EdInt([byte[]]$b) {
    $le = New-Object byte[] ($b.Length + 1) # a trailing 0: never negative
    [Array]::Copy($b, $le, $b.Length)
    return [System.Numerics.BigInteger]::new($le)
}

# x's square root for y, of parity sign, or $null (RFC 8032 5.1.3).
function Get-EdX([System.Numerics.BigInteger]$y, [int]$sign) {
    $pp = $script:EdP
    $yy = [System.Numerics.BigInteger]::Remainder($y * $y, $pp)
    $u = $yy - 1 + $pp
    $v = [System.Numerics.BigInteger]::Remainder($script:EdD * $yy + 1, $pp)
    $x2 = [System.Numerics.BigInteger]::Remainder($u * [System.Numerics.BigInteger]::ModPow($v, $pp - 2, $pp), $pp)
    if ($x2.IsZero) {
        if ($sign -eq 1) { return $null }
        return $x2
    }
    $x = [System.Numerics.BigInteger]::ModPow($x2, [System.Numerics.BigInteger]::Divide($pp + 3, 8), $pp)
    if (-not [System.Numerics.BigInteger]::Remainder($x * $x - $x2, $pp).IsZero) {
        $x = [System.Numerics.BigInteger]::Remainder($x * $script:EdI, $pp)
    }
    if (-not [System.Numerics.BigInteger]::Remainder($x * $x - $x2, $pp).IsZero) { return $null }
    if ([int](-not $x.IsEven) -ne $sign) { $x = $pp - $x }
    return $x
}

# A point from its 32-byte encoding, or $null.
function ConvertFrom-EdBytes([byte[]]$b) {
    $c = [byte[]]$b.Clone()
    $sign = ($c[31] -shr 7) -band 1
    $c[31] = $c[31] -band 0x7f
    $y = ConvertTo-EdInt $c
    if ($y -ge $script:EdP) { return $null }
    $x = Get-EdX $y $sign
    if ($null -eq $x) { return $null }
    return , @($x, $y, [System.Numerics.BigInteger]::One, [System.Numerics.BigInteger]::Remainder($x * $y, $script:EdP))
}

function ConvertTo-EdBytes($P) {
    $pp = $script:EdP
    $zi = [System.Numerics.BigInteger]::ModPow($P[2] + $pp, $pp - 2, $pp)
    $x = [System.Numerics.BigInteger]::Remainder([System.Numerics.BigInteger]::Remainder($P[0] * $zi, $pp) + $pp, $pp)
    $y = [System.Numerics.BigInteger]::Remainder([System.Numerics.BigInteger]::Remainder($P[1] * $zi, $pp) + $pp, $pp)
    $out = New-Object byte[] 32
    $yb = $y.ToByteArray()
    [Array]::Copy($yb, $out, [Math]::Min(32, $yb.Length))
    if (-not $x.IsEven) { $out[31] = $out[31] -bor 0x80 }
    return , $out
}

# add-2008-hwcd-3, a = -1 (RFC 8032 5.1.4).
function Add-EdPoint($P, $Q) {
    $pp = $script:EdP
    $A = [System.Numerics.BigInteger]::Remainder(($P[1] - $P[0]) * ($Q[1] - $Q[0]), $pp)
    $B = [System.Numerics.BigInteger]::Remainder(($P[1] + $P[0]) * ($Q[1] + $Q[0]), $pp)
    $C = [System.Numerics.BigInteger]::Remainder([System.Numerics.BigInteger]::Remainder($P[3] * $Q[3], $pp) * $script:EdD2, $pp)
    $D = [System.Numerics.BigInteger]::Remainder(2 * $P[2] * $Q[2], $pp)
    $E = $B - $A; $F = $D - $C; $G = $D + $C; $H = $B + $A
    return , @([System.Numerics.BigInteger]::Remainder($E * $F, $pp), [System.Numerics.BigInteger]::Remainder($G * $H, $pp),
        [System.Numerics.BigInteger]::Remainder($F * $G, $pp), [System.Numerics.BigInteger]::Remainder($E * $H, $pp))
}

# dbl-2008-hwcd, a = -1.
function Add-EdDouble($P) {
    $pp = $script:EdP
    $A = [System.Numerics.BigInteger]::Remainder($P[0] * $P[0], $pp)
    $B = [System.Numerics.BigInteger]::Remainder($P[1] * $P[1], $pp)
    $C = [System.Numerics.BigInteger]::Remainder(2 * $P[2] * $P[2], $pp)
    $s = $P[0] + $P[1]
    $E = [System.Numerics.BigInteger]::Remainder($s * $s, $pp) - $A - $B
    $G = $B - $A; $F = $G - $C; $H = [System.Numerics.BigInteger]::Negate($A + $B)
    return , @([System.Numerics.BigInteger]::Remainder($E * $F, $pp), [System.Numerics.BigInteger]::Remainder($G * $H, $pp),
        [System.Numerics.BigInteger]::Remainder($F * $G, $pp), [System.Numerics.BigInteger]::Remainder($E * $H, $pp))
}

$script:EdBy = [System.Numerics.BigInteger]::Remainder(4 * [System.Numerics.BigInteger]::ModPow(5, $script:EdP - 2, $script:EdP), $script:EdP)
$script:EdB = @((Get-EdX $script:EdBy 0), $script:EdBy, [System.Numerics.BigInteger]::One,
    [System.Numerics.BigInteger]::Remainder((Get-EdX $script:EdBy 0) * $script:EdBy, $script:EdP))

# The 256 bits of a scalar below 2^256, least significant first.
function Get-EdBits([System.Numerics.BigInteger]$k) {
    $b = $k.ToByteArray()
    $bits = New-Object int[] 256
    for ($i = 0; $i -lt 256; $i++) {
        $j = $i -shr 3
        if ($j -lt $b.Length) { $bits[$i] = ($b[$j] -shr ($i -band 7)) -band 1 }
    }
    return , $bits
}

function Test-Ed25519([byte[]]$pub, [byte[]]$msg, [byte[]]$sig) {
    if ($null -eq $msg) { $msg = [byte[]]@() }
    if ($null -eq $pub -or $null -eq $sig -or $pub.Length -ne 32 -or $sig.Length -ne 64) { return $false }
    $rb = New-Object byte[] 32; [Array]::Copy($sig, 0, $rb, 0, 32)
    $sb = New-Object byte[] 32; [Array]::Copy($sig, 32, $sb, 0, 32)
    $s = ConvertTo-EdInt $sb
    if ($s -ge $script:EdL) { return $false }
    $A = ConvertFrom-EdBytes $pub
    if ($null -eq $A) { return $false }
    $sha = [System.Security.Cryptography.SHA512]::Create()
    $k = [System.Numerics.BigInteger]::Remainder((ConvertTo-EdInt $sha.ComputeHash([byte[]]($rb + $pub + $msg))), $script:EdL)
    # R' = [s]B + [k](-A), both scalars in one pass (Straus).
    $nA = @([System.Numerics.BigInteger]::Negate($A[0]), $A[1], $A[2], [System.Numerics.BigInteger]::Negate($A[3]))
    $sum = Add-EdPoint $script:EdB $nA
    $sbits = Get-EdBits $s
    $kbits = Get-EdBits $k
    $R = @([System.Numerics.BigInteger]::Zero, [System.Numerics.BigInteger]::One, [System.Numerics.BigInteger]::One, [System.Numerics.BigInteger]::Zero)
    for ($i = 255; $i -ge 0; $i--) {
        $R = Add-EdDouble $R
        if ($sbits[$i] -eq 1) {
            if ($kbits[$i] -eq 1) { $R = Add-EdPoint $R $sum } else { $R = Add-EdPoint $R $script:EdB }
        } elseif ($kbits[$i] -eq 1) { $R = Add-EdPoint $R $nA }
    }
    $got = ConvertTo-EdBytes $R
    for ($i = 0; $i -lt 32; $i++) { if ($got[$i] -ne $rb[$i]) { return $false } }
    return $true
}

# Test-Minisign: $sigFile must be a prehashed minisign signature by
# $pubKeyB64, over the file and over its trusted comment, which must name
# checksums.txt. True, or it throws the reason, in install.sh's words.
function Test-Minisign([string]$file, [string]$sigFile, [string]$pubKeyB64) {
    $f = [IO.Path]::GetFileName($file)
    $sf = [IO.Path]::GetFileName($sigFile)
    try { $pub = [Convert]::FromBase64String($pubKeyB64.Trim()) } catch { $pub = $null }
    if ($null -eq $pub -or $pub.Length -ne 42 -or $pub[0] -ne 0x45 -or $pub[1] -ne 0x64) { throw 'bad public key' }
    $text = [Text.Encoding]::UTF8.GetString([IO.File]::ReadAllBytes($sigFile))
    $lines = @($text -split "`n" | ForEach-Object { $_.TrimEnd("`r") })
    $raw = $null
    if ($lines.Count -ge 2) { try { $raw = [Convert]::FromBase64String($lines[1]) } catch { } }
    if ($null -eq $raw -or $raw.Length -ne 74) { throw "${sf}: unreadable" }
    if ($raw[0] -ne 0x45 -or $raw[1] -ne 0x44) { throw "${sf}: not a prehashed minisign signature" }
    for ($i = 2; $i -lt 10; $i++) {
        if ($raw[$i] -ne $pub[$i]) {
            $id = (9..2 | ForEach-Object { $raw[$_].ToString('X2') }) -join ''
            throw "${sf}: signed by another key (key $id)"
        }
    }
    $key = New-Object byte[] 32; [Array]::Copy($pub, 10, $key, 0, 32)
    $sig = New-Object byte[] 64; [Array]::Copy($raw, 10, $sig, 0, 64)
    if (-not (Test-Ed25519 $key (Get-Blake2b512 ([IO.File]::ReadAllBytes($file))) $sig)) { throw "${f}: bad signature" }
    $prefix = 'trusted comment: '
    if ($lines.Count -lt 3 -or -not $lines[2].StartsWith($prefix, [StringComparison]::Ordinal)) { throw "${sf}: no trusted comment" }
    $comment = $lines[2].Substring($prefix.Length)
    $gsig = $null
    if ($lines.Count -ge 4) { try { $gsig = [Convert]::FromBase64String($lines[3]) } catch { } }
    if ($null -eq $gsig -or $gsig.Length -ne 64) { throw "${sf}: unreadable" }
    if (-not (Test-Ed25519 $key ([byte[]]($sig + [Text.Encoding]::UTF8.GetBytes($comment))) $gsig)) {
        throw "${sf}: bad trusted comment signature"
    }
    # The trusted comment is signed: it must name the file it was made for.
    if (@($comment -split "`t") -cnotcontains 'file:checksums.txt') { throw "$sf was made for another file" }
    return $true
}

# Invoke-Https: a GET that must end on https, as install.sh's curl
# --proto '=https': Windows PowerShell 5.1 (.NET Framework) follows a
# redirect from https to http, so the final address is checked. The files'
# integrity never rests on it (checksums.txt is signed, the zip is listed
# in it), the newest release's tag does. $outFile: saved there, removed
# when refused.
function Invoke-Https([string]$uri, [string]$outFile, [hashtable]$headers) {
    if (-not $uri.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase)) { throw 'not https' }
    $a = @{ UseBasicParsing = $true; Uri = $uri }
    if ($headers) { $a.Headers = $headers }
    if ($outFile) { $a.OutFile = $outFile; $a.PassThru = $true }
    $r = Invoke-WebRequest @a
    $final = $r.BaseResponse.ResponseUri # Windows PowerShell 5.1
    if ($null -eq $final -and $null -ne $r.BaseResponse.RequestMessage) { $final = $r.BaseResponse.RequestMessage.RequestUri } # PowerShell 7
    if ($null -eq $final -or $final.Scheme -ne 'https') {
        if ($outFile) { Remove-Item -LiteralPath $outFile -Force -ErrorAction SilentlyContinue }
        throw "redirected to $final, not https"
    }
    return $r
}

# Get-NewestTag: the tag of the newest release, pre-releases included.
# GitHub's /releases/latest skips pre-releases; the release list, newest
# first, does not.
function Get-NewestTag([string]$api, [string]$repo, [string]$downloads) {
    $list = "$api/repos/$repo/releases?per_page=1"
    $how = "set BACKSIGHT_RELEASE_URL=$downloads/<tag> (tags: https://github.com/$repo/releases), or BACKSIGHT_RELEASE_DIR to a downloaded release"
    try { $content = (Invoke-Https $list $null @{ Accept = 'application/vnd.github+json' }).Content }
    catch { throw "cannot list the releases at ${list}: $how" }
    if ($content -is [byte[]]) { $content = [Text.Encoding]::UTF8.GetString($content) }
    $first = $null
    try { $parsed = ConvertFrom-Json -InputObject $content; $first = @($parsed)[0] } catch { }
    $tag = $null
    if ($first -isnot [string] -and $null -ne $first) { $tag = $first.tag_name }
    if (-not $tag) { throw "no release found at ${list}: $how" }
    if ($tag -isnot [string] -or $tag -cnotmatch '^[A-Za-z0-9._+-]+\z' -or $tag.StartsWith('.')) { throw "unexpected release tag at ${list}: $how" }
    return $tag
}

function Install-Backsight {
    Set-StrictMode -Off # a strict profile would refuse the absent JSON fields read below
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Windows PowerShell's progress bar slows downloads tenfold
    # The release key (decision 0027). Changed only by a key rotation.
    $pubkey = 'RWTezqkiNo2ey8msd7vpSC0+D6oghZ1/VwgXU6EqYRjlZzyd87+/03eG'
    if ($env:BACKSIGHT_INSTALL_PUBKEY) { $pubkey = $env:BACKSIGHT_INSTALL_PUBKEY }
    $repo = 'Barben360/backsight'
    $api = 'https://api.github.com'
    $github = 'https://github.com'
    if ($env:BACKSIGHT_INSTALL_GITHUB) { $api = $env:BACKSIGHT_INSTALL_GITHUB; $github = $env:BACKSIGHT_INSTALL_GITHUB }
    $downloads = "$github/$repo/releases/download"
    $url = $env:BACKSIGHT_RELEASE_URL
    $platforms = "https://github.com/$repo/blob/main/docs/compatibility.md"

    function warn([string]$m) { [Console]::Error.WriteLine("backsight install: WARNING: $m") }
    function say([string]$m) { [Console]::Out.WriteLine($m) }

    if ($env:BACKSIGHT_INSTALL_PUBKEY) {
        warn "BACKSIGHT_INSTALL_PUBKEY is set: the release is checked against $pubkey, not backsight's release key (a test setting: unset it unless you are testing)"
    }
    if ($env:BACKSIGHT_INSTALL_GITHUB) {
        warn "BACKSIGHT_INSTALL_GITHUB is set: the release comes from $($env:BACKSIGHT_INSTALL_GITHUB), not GitHub (a test setting: unset it unless you are testing)"
    }

    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        $system = [Environment]::OSVersion.Platform
        if ($IsLinux) { $system = 'Linux' } elseif ($IsMacOS) { $system = 'macOS' }
        throw "unsupported system ${system}: install.ps1 installs backsight on Windows; on Linux and macOS, install.sh does (platforms: $platforms)"
    }
    $machine = $env:PROCESSOR_ARCHITEW6432 # a 32-bit PowerShell on a 64-bit Windows
    if (-not $machine) { $machine = $env:PROCESSOR_ARCHITECTURE }
    if ($machine -ne 'AMD64') { throw "unsupported machine ${machine}: backsight runs on Windows x86_64 only for now (platforms: $platforms)" }
    $os = 'windows'
    $arch = 'amd64'

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('backsight-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        if (-not $env:BACKSIGHT_RELEASE_DIR) {
            # Windows PowerShell 5.1 may not offer TLS 1.2 by default.
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        }
        # fetch <name>: into $tmp
        function fetch([string]$name) {
            if ($env:BACKSIGHT_RELEASE_DIR) {
                $src = Join-Path $env:BACKSIGHT_RELEASE_DIR $name
                if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { throw "$name is not in $($env:BACKSIGHT_RELEASE_DIR)" }
                Copy-Item -LiteralPath $src -Destination (Join-Path $tmp $name)
                return
            }
            try { Invoke-Https "$url/$name" (Join-Path $tmp $name) $null | Out-Null }
            catch { throw "cannot download $url/${name}: $($_.Exception.Message)" }
        }

        if (-not $env:BACKSIGHT_RELEASE_DIR -and -not $url) {
            $tag = Get-NewestTag $api $repo $downloads
            say "newest release: $tag"
            $url = "$downloads/$tag"
        }

        fetch 'checksums.txt'
        fetch 'checksums.txt.minisig'
        Test-Minisign (Join-Path $tmp 'checksums.txt') (Join-Path $tmp 'checksums.txt.minisig') $pubkey | Out-Null

        $sums = [IO.File]::ReadAllLines((Join-Path $tmp 'checksums.txt'))
        $suffix = "_${os}_${arch}.zip"
        $archive = $null
        foreach ($l in $sums) {
            $w = -split $l
            if ($w.Count -ge 2 -and $w[1].StartsWith('backsight_', [StringComparison]::Ordinal) -and $w[1].EndsWith($suffix, [StringComparison]::Ordinal)) { $archive = $w[1]; break }
        }
        if (-not $archive) { throw "the release has no archive for $os/$arch" }
        if ($archive -match '[/\\:]' -or $archive.StartsWith('.')) { throw "unexpected archive name $archive" }
        $want = $null
        foreach ($l in $sums) { $w = -split $l; if ($w.Count -ge 2 -and $w[1] -ceq $archive) { $want = $w[0]; break } }
        fetch $archive
        $sha = [Security.Cryptography.SHA256]::Create()
        $got = ($sha.ComputeHash([IO.File]::ReadAllBytes((Join-Path $tmp $archive))) | ForEach-Object { $_.ToString('x2') }) -join ''
        if ($got -cne $want) { throw "${archive}: checksum mismatch" }

        $version = $archive.Substring('backsight_'.Length, $archive.Length - 'backsight_'.Length - $suffix.Length)
        # The version names the install directory, <data>\bin\<version>.
        if ($version -cnotmatch '^[A-Za-z0-9._+-]+\z' -or $version.StartsWith('.')) { throw "unexpected release version in $archive" }
        $x = Join-Path $tmp 'x'
        New-Item -ItemType Directory -Path $x | Out-Null
        $exe = Join-Path $x 'backsight.exe'
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        try { $zip = [IO.Compression.ZipFile]::OpenRead((Join-Path $tmp $archive)) }
        catch { throw "$archive is not a zip archive" }
        try {
            $entries = @($zip.Entries | Where-Object { $_.FullName -ceq 'backsight.exe' })
            if ($entries.Count -ne 1) { throw "$archive holds no backsight.exe" }
            [IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0], $exe)
        } finally { $zip.Dispose() }
        # The binary checks itself against the signed checksums on its
        # first run: they go with it.
        foreach ($f in 'checksums.txt', 'checksums.txt.minisig') { Copy-Item -LiteralPath (Join-Path $tmp $f) -Destination (Join-Path $x $f) }

        # The verified binary installs itself: <data>\bin\<version>, the
        # shim current.exe every generated file runs, the command on PATH
        # (a copy of the shim: a symlink needs a privilege a standard user
        # lacks), and <bin> on the user PATH.
        $out = & $exe __install-shim $version --json | Out-String
        $code = $LASTEXITCODE
        $res = $null
        try { $res = $out | ConvertFrom-Json } catch { }
        if ($code -ne 0 -or $null -eq $res -or -not $res.ok) {
            $why = "exit $code"
            if ($null -ne $res -and $null -ne $res.error) { $why = $res.error.message }
            throw "cannot install backsight ${version}: $why"
        }
        $bin = $res.data.bin
        $command = $res.data.command # none: the user's own file is there, left alone
        if (-not $command) { $command = $res.data.shim }
        say "backsight $version installed: $command"
        $here = @($env:PATH -split ';' | Where-Object { $_.TrimEnd('\') -eq $bin.TrimEnd('\') })
        if ($here.Count -eq 0) { say "$bin is on your user PATH: open a new terminal to run backsight" }
        say 'next: backsight setup'
        say 'a coding agent installing for you: backsight guide agent-guide'
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# Everything runs from Install-Backsight, called last: a download cut short
# runs nothing.
if ($env:BACKSIGHT_INSTALL_LIB -ne '1') {
    $failed = $false
    try { Install-Backsight } catch { [Console]::Error.WriteLine("backsight install: $($_.Exception.Message)"); $failed = $true }
    if ($failed) {
        # powershell -c "irm ... | iex" or -File: the exit code tells the
        # caller. Typed at a prompt, or under -NoExit, exit would close the
        # user's window.
        $hostArgs = @([Environment]::GetCommandLineArgs() | Select-Object -Skip 1)
        $scripted = @($hostArgs | Where-Object { $_ -match '^[-/](c|f|e|ec|com\w*|fi\w*|enc\w*)$' }).Count -gt 0
        $noExit = @($hostArgs | Where-Object { $_ -match '^[-/]noe\w*$' }).Count -gt 0
        if ($scripted -and -not $noExit) { exit 1 }
        $global:LASTEXITCODE = 1
    }
}
