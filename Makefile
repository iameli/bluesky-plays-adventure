
adv550.wasm:
	clang adv550.c advdat.c --target=wasm32-unknown-wasi -I /usr/include/wasm32-wasi -L /usr/lib/wasm32-wasi -nodefaultlibs -Wl,-lc -o adv550.wasm