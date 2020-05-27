# goos-e
Goose is a GO Lang based operating system - its experemental

### Installation steps

#### step 1: Install buildutils

`
sudo apt-get install -y binutils-common`

#### step 2: Install xorriso

`
sudo apt-get install -y xorriso`

#### step 3: Install grub

`
sudo apt-get install grub-pc`

#### step 4: Install nasm

`
sudo apt-get install -y nasm`

#### step 5: Install GOLang

`
sudo apt-get remove golang --purge

sudo add-apt-repository ppa:gophers/archive
sudo apt-get update

sudo apt-get install golang-1.8

sudo ln /usr/lib/go-1.8/bin/go /usr/bin/go1.8
`

#### step 6: Install qemu

`
sudo wget https://download.qemu.org/qemu-5.0.0.tar.xz
sudo tar xvJf qemu-5.0.0.tar.xz
sudo cd qemu-5.0.0
sudo ./configure
sudo make
`

### Buld the project

`
make run-qemu
`
