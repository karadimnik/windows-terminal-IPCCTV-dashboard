<#
    golden-relay.ps1 - the generated MediaMTX configs of the PowerShell product:
    the recording relay (Get-CamRelayConfigText, from mediamtx.template.yml) and
    the live-view instance (Get-CamLiveConfigText). Dot-source holocron-lib.ps1
    first.

    FAKE inputs only: three cameras on documentation addresses, an API login
    camrelayapi / FAKE-API-PASSWORD-NOT-A-SECRET and a live login camlive /
    FAKE-LIVE-SECRET-NOT-A-SECRET. The configs hold only their SHA-256, which is
    the hash of those fake values.

    The files are byte-exact (CRLF, as the product writes them); .gitattributes
    marks testdata/golden as -text so git never rewrites them.
#>

function New-HolocronRelayGoldens {
    param([Parameter(Mandatory = $true)][string]$OutDir)

    foreach ($sub in @('relay', 'live')) {
        $dir = Join-Path -Path $OutDir -ChildPath $sub
        if (Test-Path -LiteralPath $dir) { Get-ChildItem -LiteralPath $dir -File | Remove-Item -Force }
        [void](New-Item -ItemType Directory -Path $dir -Force)
    }

    $apiUser = 'camrelayapi'; $apiPassword = 'FAKE-API-PASSWORD-NOT-A-SECRET'
    $liveUser = 'camlive'; $liveSecret = 'FAKE-LIVE-SECRET-NOT-A-SECRET'
    $apiCred = New-Object System.Management.Automation.PSCredential($apiUser, (ConvertTo-SecureString $apiPassword -AsPlainText -Force))
    $liveCred = New-Object System.Management.Automation.PSCredential($liveUser, (ConvertTo-SecureString $liveSecret -AsPlainText -Force))

    $cameras = @(
        [pscustomobject]@{ Name = 'Camera One'; IP = '192.0.2.11'; Quality = 'stream1'; Record = $true },
        [pscustomobject]@{ Name = 'Camera Two'; IP = '192.0.2.12'; Quality = 'stream2'; Record = $true },
        [pscustomobject]@{ Name = 'Camera Three'; IP = '192.0.2.13'; Record = $false })
    $relayMap = Get-CamRelayPathMap -Cameras $cameras

    $liveKeys = @{ LiveViewEnabled = $true; LiveSubStream = 'stream2'; LiveCloseAfterSeconds = 7 }
    $variants = [ordered]@{
        'relay/default.yml'            = @{ Kind = 'relay'; Settings = @{}; Description = 'default configuration' }
        'relay/live-off.yml'           = @{ Kind = 'relay'; Settings = @{ LiveViewEnabled = $false; LiveSubStream = 'stream2'; LiveCloseAfterSeconds = 7 }; Description = 'live view explicitly off with live keys set: must equal default.yml (and master)' }
        'relay/live-on.yml'            = @{ Kind = 'relay'; Settings = $liveKeys; Description = 'live view on: the RELAY config must not change: must equal default.yml' }
        'relay/custom-addresses.yml'   = @{ Kind = 'relay'; Settings = @{ RelayBase = 'rtsp://127.0.0.1:18554'; RelayApiBase = 'http://127.0.0.1:19997' }; Description = 'other loopback ports for the RTSP and API listeners' }
        'live/default.yml'             = @{ Kind = 'live'; Settings = @{ LiveViewEnabled = $true }; Description = 'live view on, all defaults' }
        'live/sub-stream-and-close.yml' = @{ Kind = 'live'; Settings = $liveKeys; Description = 'LiveSubStream stream2 (camera Two already records stream2, so it gets only a main path), close after 7 s' }
        'live/lan.yml'                 = @{ Kind = 'live'; Settings = @{ LiveViewEnabled = $true; LiveWhepAddress = '127.0.0.1:18889'; LiveUdpAddress = '0.0.0.0:18189'; LiveAdvertiseHosts = @('192.0.2.50', '192.0.2.51') }; Description = 'media port on all interfaces and two advertised LAN addresses' }
    }

    $rendered = [ordered]@{}
    $files = [ordered]@{}
    foreach ($name in $variants.Keys) {
        $v = $variants[$name]
        $config = Get-CamConfigDefault
        foreach ($key in $v.Settings.Keys) { $config[$key] = $v.Settings[$key] }
        if ($v.Kind -eq 'relay') {
            $text = Get-CamRelayConfigText -Config $config -PathMap $relayMap -ApiCredential $apiCred
        }
        else {
            $text = Get-CamLiveConfigText -Config $config -LivePathMap (Get-CamLivePathMap -Config $config -RelayPathMap $relayMap) -LiveCredential $liveCred
        }
        if ($text.Length -lt 500 -or $text -notmatch "`r`n") { throw "config $name looks empty or not CRLF" }
        Save-HolocronText -Path (Join-Path $OutDir $name) -Text $text
        $rendered[$name] = $text
        $files[$name] = $v.Description
    }

    # The recording relay does not change when live view is switched on or off.
    if ($rendered['relay/live-off.yml'] -ne $rendered['relay/default.yml'] -or $rendered['relay/live-on.yml'] -ne $rendered['relay/default.yml']) {
        throw 'the relay config depends on the live-view flag'
    }
    # No secret in any config, only the hashes.
    foreach ($name in $rendered.Keys) {
        if ($rendered[$name].Contains($apiPassword) -or $rendered[$name].Contains($liveSecret)) { throw "config $name contains a fake secret in clear" }
    }

    $inputs = [ordered]@{
        fixture      = 'SYNTHETIC FIXTURE: fake cameras on documentation addresses and fake logins. Not a leak.'
        description  = 'The inputs behind every file in relay/ and live/; the YAML files are the expected output, byte for byte (CRLF).'
        cameras      = @($cameras | ForEach-Object { [ordered]@{ Name = $_.Name; IP = $_.IP; Quality = [string]$_.Quality; Record = $_.Record } })
        apiLogin     = [ordered]@{ user = $apiUser; password = $apiPassword; passwordHash = (Get-CamRelayApiPassHash -Credential $apiCred) }
        liveLogin    = [ordered]@{ user = $liveUser; secret = $liveSecret; secretHash = (Get-CamRelayApiPassHash -Credential $liveCred) }
        relayPaths   = @($relayMap.Keys | ForEach-Object { [string]$_ })
        template     = 'mediamtx.template.yml (repository root)'
        templateSha256 = (Get-FileHash -LiteralPath (Join-Path (Get-HolocronRepo) 'mediamtx.template.yml') -Algorithm SHA256).Hash.ToLowerInvariant()
        variants     = @($variants.Keys | ForEach-Object {
                $v = $variants[$_]
                $settings = [ordered]@{}
                foreach ($k in ($v.Settings.Keys | Sort-Object)) { $settings[$k] = $v.Settings[$k] }
                [ordered]@{ file = $_; kind = $v.Kind; description = $v.Description; settings = $settings }
            })
    }
    Save-HolocronText -Path (Join-Path $OutDir 'relay-live-inputs.json') -Text (ConvertTo-HolocronJson -InputObject $inputs)
}
