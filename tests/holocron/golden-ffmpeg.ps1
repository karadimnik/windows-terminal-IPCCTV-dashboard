<#
    golden-ffmpeg.ps1 - the exact ffmpeg argument vectors of every recorder
    variant, as the PowerShell product builds them (Get-CamFfmpegArgs, with the
    input chosen by Get-CamInputUrl and the pattern by Get-CamSegmentPattern),
    plus the quoting layer (Get-CamQuotedArgument / Get-CamCommandLine) that the
    Go port does not need but must be able to explain.

    Dot-source holocron-lib.ps1 first. All inputs are FAKE: camera 'Camera One'
    at 192.0.2.11, user fakeuser, password fakepass. The direct-input files carry
    that fake credential in the URL; every file says so in "fixture".
#>

$script:HolocronFixtureNote = 'SYNTHETIC FIXTURE: fake camera (documentation address 192.0.2.11), fake credential fakeuser / fakepass. Not a leak.'


function New-HolocronFfmpegVariant {
    param(
        [string]$Id, [string]$Description,
        [ValidateSet('mkv', 'fmp4')][string]$Container = 'mkv',
        [ValidateSet('relay', 'direct')][string]$InputMode = 'relay',
        [int]$SegmentSeconds = 900,
        [string]$Root = 'C:\holocron-fixture\cameras',
        [hashtable]$ConfigOverride = @{},
        $ProfileOverride = $null
    )

    $config = Get-CamConfigDefault
    $config.RootPath = $Root
    $config.Container = $Container
    $config.SegmentSeconds = $SegmentSeconds
    foreach ($key in $ConfigOverride.Keys) { $config[$key] = $ConfigOverride[$key] }
    if ($ProfileOverride) { $config.ContainerProfiles[$Container] = $ProfileOverride }
    $profile = Get-CamContainerProfile -Config $config

    $camera = [pscustomobject]@{ Name = 'Camera One'; IP = '192.0.2.11'; Quality = 'stream1'; Record = $true }
    $safe = Get-CamSafeName -Name $camera.Name
    $relayPath = Get-CamRelayPathName -SafeName $safe
    $cameraDir = Join-Path -Path $Root -ChildPath $safe
    $pattern = Get-CamSegmentPattern -CameraDir $cameraDir -SafeName $safe -ContainerProfile $profile
    $credential = New-Object System.Management.Automation.PSCredential('fakeuser', (ConvertTo-SecureString 'fakepass' -AsPlainText -Force))
    $directUrl = Get-CamRtspUrl -Camera $camera -Credential $credential

    # The same decision the supervisor makes per start: the relay when its path is in the ready set.
    $ready = $(if ($InputMode -eq 'relay') { @($relayPath) } else { @() })
    $choice = Get-CamInputUrl -Config $config -RelayPathName $relayPath -DirectUrl $directUrl -ReadyPaths $ready
    if ($choice.Mode -ne $InputMode) { throw "variant $Id wanted input '$InputMode' but the product chose '$($choice.Mode)'" }

    $arguments = Get-CamFfmpegArgs -Url $choice.Url -OutputPattern $pattern -Config $config -ContainerProfile $profile

    return [ordered]@{
        fixture     = $script:HolocronFixtureNote
        id          = $Id
        description = $Description
        inputs      = [ordered]@{
            container          = $Container
            segmentSeconds     = $SegmentSeconds
            inputMode          = $choice.Mode
            ffmpegPath         = [string]$config.FfmpegPath
            archiveRoot        = $Root
            cameraName         = $camera.Name
            cameraIP           = $camera.IP
            quality            = $camera.Quality
            safeName           = $safe
            relayPath          = $relayPath
            relayBase          = [string]$config.RelayBase
            rtspTransport      = [string]$config.RtspTransport
            timeoutMicroseconds = [int64]$config.TimeoutMicroseconds
            url                = $choice.Url
            outputPattern      = $pattern
            containerProfile   = [ordered]@{
                Extension            = [string]$profile.Extension
                SegmentFormat        = [string]$profile.SegmentFormat
                AudioArgs            = @($profile.AudioArgs | ForEach-Object { [string]$_ })
                ExtraArgs            = @($profile.ExtraArgs | ForEach-Object { [string]$_ })
                SegmentFormatOptions = [string]$profile.SegmentFormatOptions
            }
        }
        args        = @($arguments | ForEach-Object { [string]$_ })
        commandLine = (Get-CamCommandLine -Arguments $arguments)
    }
}


function New-HolocronFfmpegGoldens {
    param([Parameter(Mandatory = $true)][string]$OutDir)

    $dir = Join-Path -Path $OutDir -ChildPath 'ffmpeg'
    if (Test-Path -LiteralPath $dir) { Get-ChildItem -LiteralPath $dir -Filter '*.json' | Remove-Item -Force }
    [void](New-Item -ItemType Directory -Path $dir -Force)

    $variants = @()
    foreach ($container in @('mkv', 'fmp4')) {
        foreach ($mode in @('relay', 'direct')) {
            foreach ($seconds in @(900, 7)) {
                $variants += @{ Id = "$container-$mode-$seconds"; Description = "$container container, $mode input, $seconds s segments"
                    Container = $container; InputMode = $mode; SegmentSeconds = $seconds }
            }
        }
    }
    $variants += @{ Id = 'mkv-relay-900-spacepath'; Description = 'archive root with a space: the pattern argument is quoted on the command line'
        Container = 'mkv'; InputMode = 'relay'; SegmentSeconds = 900; Root = 'C:\holocron fixture\cam eras' }
    $variants += @{ Id = 'mkv-direct-900-udp'; Description = 'RtspTransport udp and a 5 s socket timeout'
        Container = 'mkv'; InputMode = 'direct'; SegmentSeconds = 900; ConfigOverride = @{ RtspTransport = 'udp'; TimeoutMicroseconds = 5000000 } }
    $variants += @{ Id = 'mkv-relay-900-extraargs'; Description = 'a profile with AudioArgs, ExtraArgs and SegmentFormatOptions all set (order of the three in the vector)'
        Container = 'mkv'; InputMode = 'relay'; SegmentSeconds = 900
        ProfileOverride = [pscustomobject]@{ Extension = '.mkv'; SegmentFormat = 'matroska'; AudioArgs = @('-c:a', 'aac'); ExtraArgs = @('-metadata', 'title=Camera One'); SegmentFormatOptions = 'live=1' } }

    foreach ($v in $variants) {
        $splat = @{ Id = $v.Id; Description = $v.Description; Container = $v.Container; InputMode = $v.InputMode; SegmentSeconds = $v.SegmentSeconds }
        if ($v.Root) { $splat.Root = $v.Root }
        if ($v.ConfigOverride) { $splat.ConfigOverride = $v.ConfigOverride }
        if ($v.ProfileOverride) { $splat.ProfileOverride = $v.ProfileOverride }
        $doc = New-HolocronFfmpegVariant @splat
        Save-HolocronText -Path (Join-Path $dir ($v.Id + '.json')) -Text (ConvertTo-HolocronJson -InputObject $doc)
    }

    # The quoting layer: CommandLineToArgvW rules, one row per awkward argument.
    $samples = @('plain', '', 'two words', ('tab' + [char]9 + 'here'), 'say "hi"', 'C:\dir\', 'C:\dir with space\', 'C:\dir with space\x\', 'a\"b', '\\server\share x', '100%', 'rtsp://fakeuser:fakepass@192.0.2.11/stream1')
    $rows = @()
    foreach ($s in $samples) { $rows += [ordered]@{ argument = $s; quoted = (Get-CamQuotedArgument -Value $s) } }
    $quoting = [ordered]@{
        fixture = $script:HolocronFixtureNote
        description = 'Get-CamQuotedArgument rows, and Get-CamCommandLine over every non-empty row (the product cannot pass an empty argument through Get-CamCommandLine)'
        rows = $rows
        commandLine = (Get-CamCommandLine -Arguments @($samples | Where-Object { $_ -ne '' }))
    }
    Save-HolocronText -Path (Join-Path $dir 'quoting.json') -Text (ConvertTo-HolocronJson -InputObject $quoting)
}
