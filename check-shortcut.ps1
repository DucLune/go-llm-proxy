$ws = New-Object -ComObject WScript.Shell
$startup = [Environment]::GetFolderPath('Startup')
$lnk = $ws.CreateShortcut("$startup\start-hidden.vbs - 快捷方式.lnk")
Write-Output "TargetPath: $($lnk.TargetPath)"
Write-Output "Arguments: $($lnk.Arguments)"
Write-Output "WorkingDir: $($lnk.WorkingDirectory)"
Write-Output "IconLocation: $($lnk.IconLocation)"
