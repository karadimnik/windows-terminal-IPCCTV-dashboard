<#
    golden-names.ps1 - the camera safe-name corpus (Get-CamSafeName and
    Get-CamRelayPathName over awkward names) and the segment file name
    contract (Get-CamSegmentPattern, Test-CamSegmentName). Dot-source
    holocron-lib.ps1 first. ASCII only: every non-ASCII character is built from
    its code point.
#>

function ConvertTo-HolocronPrintable {
    <# A string with every character outside printable ASCII written as <U+XXXX>, for the "show" column only. #>
    param([string]$Text)
    $b = New-Object System.Text.StringBuilder
    foreach ($ch in $Text.ToCharArray()) {
        if ([int]$ch -ge 32 -and [int]$ch -le 126) { [void]$b.Append($ch) } else { [void]$b.AppendFormat('<U+{0:X4}>', [int]$ch) }
    }
    return $b.ToString()
}


function Get-HolocronNameCorpus {
    $u = { param([int[]]$Codes) -join ($Codes | ForEach-Object { [char]$_ }) }
    $corpus = [ordered]@{}
    $add = { param([string]$Id, [string]$Name) $corpus[$Id] = $Name }

    & $add 'ascii-simple' 'Lobby Entrance'
    & $add 'ascii-underscore-dash' 'Yard_Shed-2'
    & $add 'spaces-trim-and-collapse' '  Back   Porch  '
    & $add 'tab-and-newline' ("A" + [char]9 + "B" + [char]10 + "C")
    & $add 'unicode-nbsp' ('A' + (& $u 0x00A0) + 'B')
    & $add 'unicode-em-space' ('A' + (& $u 0x2003) + 'B')
    & $add 'unicode-ideographic-space' ('A' + (& $u 0x3000) + 'B')
    & $add 'unicode-line-separator' ('A' + (& $u 0x2028) + 'B')
    & $add 'unicode-next-line' ('A' + (& $u 0x0085) + 'B')
    & $add 'unicode-zero-width-space' ('A' + (& $u 0x200B) + 'B')
    & $add 'unicode-leading-trailing-nbsp' ((& $u 0x00A0) + 'Gate' + (& $u 0x00A0))
    & $add 'trailing-dot' 'Garage.'
    & $add 'trailing-dots-and-spaces' 'Garage . .  '
    & $add 'inner-dot' 'Garage.North'
    & $add 'leading-dot' '.hidden'
    & $add 'percent' '100% Cam'
    & $add 'strftime-injection' '%Y%m%d'
    & $add 'percent-z' 'Cam%z'
    & $add 'leading-underscore' '_hidden'
    & $add 'double-leading-underscore' '__x'
    & $add 'underscore-only' '___'
    & $add 'reserved-con' 'CON'
    & $add 'reserved-con-lower' 'con'
    & $add 'reserved-con-extension' 'Con.txt'
    & $add 'reserved-com1' 'COM1'
    & $add 'reserved-lpt9' 'lpt9'
    & $add 'reserved-aux-trailing-dot' 'aux.'
    & $add 'reserved-nul-spaced' ' nul '
    & $add 'reserved-nul-space-dot-ext' 'NUL . x'
    & $add 'not-reserved-console' 'CONSOLE'
    & $add 'not-reserved-com0' 'COM0'
    & $add 'not-reserved-com10' 'COM10'
    & $add 'not-reserved-con-inner' 'My CON'
    & $add 'illegal-characters' 'a<b>c:d"e/f\g|h?i*j'
    & $add 'only-illegal-characters' '<>:"/\|?*'
    & $add 'control-characters' ('a' + [char]1 + 'b' + [char]127 + 'c')
    & $add 'only-control-characters' ([string][char]1 + [char]2)
    & $add 'empty' ''
    & $add 'only-spaces' '   '
    & $add 'only-dots' '...'
    & $add 'single-dot' '.'
    & $add 'long-300' ('x' * 300)
    & $add 'long-255' ('y' * 255)
    & $add 'long-with-spaces' (('word ' * 80).Trim())
    & $add 'case-collision-a' 'Front Door'
    & $add 'case-collision-b' 'front door'
    & $add 'case-collision-c' 'FRONT DOOR'
    & $add 'relay-collision-dash' 'Front-Door'
    & $add 'relay-collision-joined' 'FrontDoor'
    & $add 'digits-only' '2026'
    & $add 'greek' (& $u 0x0395,0x03AF,0x03C3,0x03BF,0x03B4,0x03BF,0x03C2)
    & $add 'greek-and-ascii' ('Gate ' + (& $u 0x0391,0x03B2))
    & $add 'cyrillic' (& $u 0x041A,0x0430,0x043C,0x0435,0x0440,0x0430)
    & $add 'cjk' (& $u 0x524D,0x9580)
    & $add 'hebrew' (& $u 0x05E9,0x05E2,0x05E8)
    & $add 'emoji' ('Cam ' + [char]::ConvertFromUtf32(0x1F4F7))
    & $add 'combining-acute' ('Cafe' + (& $u 0x0301))
    & $add 'precomposed-e-acute' ('Caf' + (& $u 0x00E9))
    & $add 'fullwidth-letters' (& $u 0xFF21,0xFF42,0xFF43)
    & $add 'turkish-dotted-capital-i' ('Gar' + (& $u 0x0130) + 'ge')
    & $add 'kelvin-sign' ('K' + (& $u 0x212A) + 'lvin')
    return $corpus
}


function Get-HolocronSafeNameRow {
    param([string]$Id, [string]$Name)

    $row = [ordered]@{ id = $Id; name = $Name; show = (ConvertTo-HolocronPrintable -Text $Name) }
    try {
        $safe = Get-CamSafeName -Name $Name
        $row.ok = $true
        $row.safeName = $safe
        $row.safeNameLength = $safe.Length
        try { $row.relayPath = Get-CamRelayPathName -SafeName $safe; $row.relayPathOk = $true }
        catch { $row.relayPathOk = $false; $row.relayPath = $null; $row.relayPathError = 'no-letter-or-digit' }
    }
    catch {
        $message = $_.Exception.Message
        $row.ok = $false
        $row.safeName = $null
        $row.errorKind = $(if ($message -like '*reserved Windows device name*') { 'reserved-device-name' } elseif ($message -like '*no characters that are legal*') { 'no-legal-characters' } else { 'other' })
        $row.error = $message
    }
    return $row
}


function Format-HolocronSegmentName {
    <# The leaf pattern with the six strftime fields replaced the way ffmpeg writes them for a LOCAL time and a +hhmm offset. #>
    param([string]$Pattern, [DateTime]$Local, [string]$Offset)
    return $Pattern.Replace('%Y', $Local.ToString('yyyy')).Replace('%m', $Local.ToString('MM')).Replace('%d', $Local.ToString('dd')).
        Replace('%H', $Local.ToString('HH')).Replace('%M', $Local.ToString('mm')).Replace('%S', $Local.ToString('ss')).Replace('%z', $Offset)
}


function New-HolocronNameGoldens {
    param([Parameter(Mandatory = $true)][string]$OutDir)

    $dir = Join-Path -Path $OutDir -ChildPath 'names'
    if (Test-Path -LiteralPath $dir) { Get-ChildItem -LiteralPath $dir -File | Remove-Item -Force }
    [void](New-Item -ItemType Directory -Path $dir -Force)

    # ---- safe names
    $corpus = Get-HolocronNameCorpus
    $rows = @()
    foreach ($id in $corpus.Keys) { $rows += (Get-HolocronSafeNameRow -Id $id -Name $corpus[$id]) }

    $byLower = @{}
    $byPath = @{}
    foreach ($r in $rows) {
        if (-not $r.ok) { continue }
        $k = $r.safeName.ToLowerInvariant()
        if (-not $byLower.ContainsKey($k)) { $byLower[$k] = @() }
        $byLower[$k] += $r.id
        if ($r.relayPathOk) {
            if (-not $byPath.ContainsKey($r.relayPath)) { $byPath[$r.relayPath] = @() }
            $byPath[$r.relayPath] += $r.id
        }
    }
    $caseGroups = @($byLower.Keys | Sort-Object | Where-Object { @($byLower[$_]).Count -gt 1 } | ForEach-Object { [ordered]@{ folder = $_; ids = @($byLower[$_]) } })
    $pathGroups = @($byPath.Keys | Sort-Object | Where-Object { @($byPath[$_]).Count -gt 1 } | ForEach-Object { [ordered]@{ relayPath = $_; ids = @($byPath[$_]) } })

    $doc = [ordered]@{
        fixture     = 'SYNTHETIC CORPUS: invented camera names. The expected values are what Get-CamSafeName and Get-CamRelayPathName return today.'
        description = 'name -> safeName (the camera folder and file prefix) or the error kind; relayPath is the MediaMTX path name derived from it. caseInsensitiveFolderCollisions and relayPathCollisions list ids whose results collide (the product refuses the second camera by throwing in Get-CamRelayPathMap for relay collisions).'
        reservedNames = @($script:CamReservedNames)
        cases       = $rows
        caseInsensitiveFolderCollisions = $caseGroups
        relayPathCollisions = $pathGroups
    }
    Save-HolocronText -Path (Join-Path $dir 'safe-names.json') -Text (ConvertTo-HolocronJson -InputObject $doc)
    New-HolocronSegmentGoldens -OutDir $OutDir
}


function New-HolocronSegmentGoldens {
    <#
        names/segments.json: the strftime pattern per container and safe name, the
        concrete names ffmpeg writes for sample instants (the substitution is
        done here the way ffmpeg's strftime does it; ffmpeg is NOT run), and the
        accept / reject lists of Test-CamSegmentName, computed by the product.
        A row with trap = true is a behaviour the product has that a rewrite
        would not obviously reproduce; it is recorded as observed, not endorsed.
    #>
    param([Parameter(Mandatory = $true)][string]$OutDir)

    $dir = Join-Path -Path $OutDir -ChildPath 'names'
    $config = Get-CamConfigDefault
    $arabicIndic = -join (@(0x0662, 0x0660, 0x0662, 0x0666) | ForEach-Object { [char]$_ })
    $fullwidthDigits = -join (@(0xFF12, 0xFF10, 0xFF12, 0xFF16) | ForEach-Object { [char]$_ })

    $samples = @(
        @{ Local = '2026-10-25 03:30:00'; Offset = '+0300'; Note = 'first pass through 03:30 on the fall-back day (summer time)' },
        @{ Local = '2026-10-25 03:30:00'; Offset = '+0200'; Note = 'second pass through the same wall time after the fall-back: only the offset tells them apart' },
        @{ Local = '2027-03-28 04:00:00'; Offset = '+0300'; Note = 'first segment after the spring-forward (03:00 does not exist)' },
        @{ Local = '2026-01-05 23:59:59'; Offset = '+0200'; Note = 'winter, end of day' },
        @{ Local = '2026-07-01 00:00:00'; Offset = '+0300'; Note = 'summer, start of day' },
        @{ Local = '2026-07-01 12:00:00'; Offset = '+0530'; Note = 'a non-hour offset (not this zone; the pattern must still hold)' },
        @{ Local = '2026-07-01 12:00:00'; Offset = '-0330'; Note = 'a negative non-hour offset' })

    $containers = @()
    foreach ($key in @('mkv', 'fmp4')) {
        $config.Container = $key
        $profile = Get-CamContainerProfile -Config $config
        $other = $(if ($key -eq 'mkv') { '.mp4' } else { '.mkv' })
        $ext = [string]$profile.Extension
        $perName = @()
        foreach ($safe in @('Camera_One', 'Front.Door', 'Cam%z', 'A+B')) {
            $pattern = [System.IO.Path]::GetFileName((Get-CamSegmentPattern -CameraDir '.' -SafeName $safe -ContainerProfile $profile))
            $rows = @()
            foreach ($s in $samples) {
                $local = [DateTime]::ParseExact($s.Local, 'yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture)
                $sign = $(if ($s.Offset[0] -eq '-') { -1 } else { 1 })
                $oh = $sign * [int]$s.Offset.Substring(1, 2); $om = $sign * [int]$s.Offset.Substring(3, 2); $offsetSpan = New-Object TimeSpan($oh, $om, 0)
                $name = Format-HolocronSegmentName -Pattern $pattern -Local $local -Offset $s.Offset
                $rows += [ordered]@{ local = $s.Local; offset = $s.Offset; utc = $local.Subtract($offsetSpan).ToString('yyyy-MM-dd HH:mm:ss'); name = $name; note = $s.Note }
            }
            $base = Format-HolocronSegmentName -Pattern $pattern -Local ([DateTime]'2026-10-25 03:30:00') -Offset '+0300'
            $stem = $base.Substring(0, $base.Length - $ext.Length)
            $cases = @(
                @{ Why = 'the exact name'; Name = $base; Expect = $true },
                @{ Why = 'file name in lower case (the match is case-insensitive)'; Name = $base.ToLowerInvariant(); Expect = $true },
                @{ Why = 'file name in upper case'; Name = $base.ToUpperInvariant(); Expect = $true },
                @{ Why = 'the other container extension'; Name = $stem + $other; Expect = $false },
                @{ Why = 'a .tmp suffix'; Name = $base + '.tmp'; Expect = $false },
                @{ Why = 'a second extension'; Name = $base + $ext; Expect = $false },
                @{ Why = 'one trailing character'; Name = $base + 'x'; Expect = $false },
                @{ Why = 'no extension'; Name = $stem; Expect = $false },
                @{ Why = 'offset missing'; Name = $base.Replace('+0300', ''); Expect = $false },
                @{ Why = 'offset written with a colon'; Name = $base.Replace('+0300', '+03:00'); Expect = $false },
                @{ Why = 'offset of two digits'; Name = $base.Replace('+0300', '+03'); Expect = $false },
                @{ Why = 'offset without a sign'; Name = $base.Replace('+0300', '0300'); Expect = $false },
                @{ Why = 'year of three digits'; Name = $base.Replace('_2026', '_202'); Expect = $false },
                @{ Why = 'date and time not separated'; Name = $base.Replace('20261025_033000', '20261025033000'); Expect = $false },
                @{ Why = 'another camera'; Name = $base.Replace('_20261025', '_20261025').Replace($stem.Substring(0, 3), 'Zzz'); Expect = $false },
                @{ Why = 'a character before the camera name'; Name = 'X' + $base; Expect = $false },
                @{ Why = 'leading space'; Name = ' ' + $base; Expect = $false },
                @{ Why = 'trailing space'; Name = $base + ' '; Expect = $false },
                @{ Why = 'a folder prefix'; Name = 'sub\' + $base; Expect = $false },
                @{ Why = 'a leading underscore (folders starting with _ are the product''s own)'; Name = '_' + $base; Expect = $false },
                @{ Why = 'the unexpanded pattern'; Name = $pattern; Expect = $false },
                @{ Why = 'empty'; Name = ''; Expect = $false },
                @{ Why = 'a trailing line feed: .NET $ also matches before a final newline'; Name = $base + "`n"; Expect = $null; Trap = $true },
                @{ Why = 'Arabic-Indic digits for the year: .NET \d matches any Unicode digit'; Name = $base.Replace('2026', $arabicIndic); Expect = $null; Trap = $true },
                @{ Why = 'fullwidth digits for the year'; Name = $base.Replace('2026', $fullwidthDigits); Expect = $null; Trap = $true })

            $accept = @(); $reject = @()
            foreach ($c in $cases) {
                $got = Test-CamSegmentName -Name $c.Name -SafeName $safe -Config $config
                if ($null -ne $c.Expect -and $got -ne $c.Expect) { throw "Test-CamSegmentName('$($c.Why)', $safe, $key) returned $got, expected $($c.Expect)" }
                $row = [ordered]@{ name = $c.Name; why = $c.Why; trap = [bool]$c.Trap }
                if ($got) { $accept += $row } else { $reject += $row }
            }
            # every sample name must be accepted
            foreach ($r in $rows) { if (-not (Test-CamSegmentName -Name $r.name -SafeName $safe -Config $config)) { throw "sample $($r.name) rejected" } }
            $perName += [ordered]@{ safeName = $safe; pattern = $pattern; samples = $rows; accept = $accept; reject = $reject }
        }
        $containers += [ordered]@{ container = $key; extension = $ext; segmentFormat = [string]$profile.SegmentFormat; names = $perName }
    }
    $doc = [ordered]@{
        fixture     = 'SYNTHETIC: invented camera names.'
        description = 'Segment file names. pattern is the leaf Get-CamSegmentPattern builds; samples are the names ffmpeg writes for those local times and offsets (substituted here, ffmpeg not run); accept/reject are what Test-CamSegmentName answers for the configured container. A trap row records an observed oddity.'
        containers  = $containers
    }
    Save-HolocronText -Path (Join-Path $dir 'segments.json') -Text (ConvertTo-HolocronJson -InputObject $doc)
}
